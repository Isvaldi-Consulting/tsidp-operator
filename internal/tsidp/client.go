// Package tsidp is a minimal client for the HTTP API of tsidp
// (github.com/tailscale/tsidp), Tailscale's OIDC identity provider.
//
// The operator talks to tsidp over the pod-local loopback listener
// (tsidp -local-port), which grants admin and dynamic client registration
// rights to loopback callers.
package tsidp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// ErrNotFound is returned when tsidp reports 404 for a client ID.
var ErrNotFound = errors.New("tsidp: client not found")

// Client calls the tsidp HTTP API.
type Client struct {
	// BaseURL is the root URL of the tsidp instance, without trailing
	// slash — the pod-local loopback listener, e.g. "http://127.0.0.1:8080".
	BaseURL string
	// HTTPClient is the underlying HTTP client.
	HTTPClient *http.Client
}

// FunnelClient mirrors the subset of tsidp's client object the operator
// consumes. tsidp blanks client_secret on GET and list responses; it is only
// populated in the POST /register response.
type FunnelClient struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"`
	ClientName   string   `json:"client_name,omitempty"`
	RedirectURIs []string `json:"redirect_uris,omitempty"`
}

// RegistrationRequest is the RFC 7591 dynamic client registration request
// body, limited to the fields tsidp decodes.
type RegistrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
	Contacts                []string `json:"contacts,omitempty"`
	ApplicationType         string   `json:"application_type,omitempty"`
}

// ProviderMetadata is the subset of the OIDC discovery document written into
// credential Secrets.
type ProviderMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserInfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

type apiError struct {
	Status      int
	ErrorCode   string `json:"error"`
	Description string `json:"error_description"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("tsidp: HTTP %d %s: %s", e.Status, e.ErrorCode, e.Description)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	return hc.Do(req)
}

func errFromResponse(resp *http.Response) error {
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	ae := &apiError{Status: resp.StatusCode}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err := json.Unmarshal(b, ae); err != nil || ae.ErrorCode == "" {
		ae.Description = strings.TrimSpace(string(b))
	}
	return ae
}

// Register creates a client via RFC 7591 dynamic client registration
// (POST /register). The response is the only place the plaintext
// client_secret is ever available; the caller must persist it immediately.
func (c *Client) Register(ctx context.Context, reg RegistrationRequest) (*FunnelClient, error) {
	buf, err := json.Marshal(reg)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/register", bytes.NewReader(buf), "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, errFromResponse(resp)
	}
	var fc FunnelClient
	if err := json.NewDecoder(resp.Body).Decode(&fc); err != nil {
		return nil, fmt.Errorf("tsidp: decoding /register response: %w", err)
	}
	if fc.ClientID == "" || fc.ClientSecret == "" {
		return nil, errors.New("tsidp: /register response missing client_id or client_secret")
	}
	return &fc, nil
}

// ListClients returns all registered clients. Secrets are blanked by tsidp.
func (c *Client) ListClients(ctx context.Context) ([]FunnelClient, error) {
	resp, err := c.do(ctx, http.MethodGet, "/clients/", nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errFromResponse(resp)
	}
	var out []FunnelClient
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("tsidp: decoding /clients/ response: %w", err)
	}
	return out, nil
}

// GetClient returns one client by ID. The secret is blanked by tsidp.
// Returns ErrNotFound if the ID is unknown.
func (c *Client) GetClient(ctx context.Context, id string) (*FunnelClient, error) {
	resp, err := c.do(ctx, http.MethodGet, "/clients/"+url.PathEscape(id), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errFromResponse(resp)
	}
	var fc FunnelClient
	if err := json.NewDecoder(resp.Body).Decode(&fc); err != nil {
		return nil, fmt.Errorf("tsidp: decoding /clients/{id} response: %w", err)
	}
	return &fc, nil
}

// DeleteClient removes a client. tsidp also purges the client's outstanding
// auth codes, access tokens, and refresh tokens. Returns ErrNotFound if the
// ID is unknown.
func (c *Client) DeleteClient(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/clients/"+url.PathEscape(id), nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return errFromResponse(resp)
	}
	return nil
}

// UpdateClient updates a client's name and redirect URIs in place via
// tsidp's admin form endpoint (POST /edit/{id}), preserving client_id and
// client_secret.
//
// This is tsidp's HTML UI contract, not a stable JSON API: the handler reads
// form fields "name" and "redirect_uris" (newline-separated), and — crucially
// — reports validation failures by re-rendering the form at HTTP 200. Status
// codes therefore cannot signal success; after posting, the update is
// verified by reading the client back and comparing. Replace this with
// PUT /clients/{id} if/when tsidp grows one.
func (c *Client) UpdateClient(ctx context.Context, id, name string, redirectURIs []string) error {
	// Mirror the server's normalization (TrimSpace on the name and each
	// URI line) so verification compares like with like.
	name = strings.TrimSpace(name)
	uris := make([]string, 0, len(redirectURIs))
	for _, u := range redirectURIs {
		if u = strings.TrimSpace(u); u != "" {
			uris = append(uris, u)
		}
	}
	form := url.Values{
		"name":          {name},
		"redirect_uris": {strings.Join(uris, "\n")},
	}
	resp, err := c.do(ctx, http.MethodPost, "/edit/"+url.PathEscape(id), strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) //nolint:errcheck
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &apiError{Status: resp.StatusCode, Description: "update via /edit failed"}
	}
	got, err := c.GetClient(ctx, id)
	if err != nil {
		return fmt.Errorf("tsidp: verifying update of %s: %w", id, err)
	}
	if got.ClientName != name || !sameSet(got.RedirectURIs, uris) {
		return fmt.Errorf("tsidp: update of client %s did not converge (server rejected it, e.g. a redirect URI scheme tsidp's UI validation forbids); remote name=%q uris=%v", id, got.ClientName, got.RedirectURIs)
	}
	return nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := slices.Clone(a), slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(as, bs)
}

// Discovery fetches the OIDC provider metadata. Even over the loopback
// listener, tsidp reports its public tailnet issuer. Uncached — it is a
// static document served from memory over loopback.
func (c *Client) Discovery(ctx context.Context) (*ProviderMetadata, error) {
	resp, err := c.do(ctx, http.MethodGet, "/.well-known/openid-configuration", nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errFromResponse(resp)
	}
	var pm ProviderMetadata
	if err := json.NewDecoder(resp.Body).Decode(&pm); err != nil {
		return nil, fmt.Errorf("tsidp: decoding discovery document: %w", err)
	}
	if pm.Issuer == "" {
		return nil, errors.New("tsidp: discovery document missing issuer")
	}
	return &pm, nil
}
