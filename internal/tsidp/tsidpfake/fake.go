// Package tsidpfake is an in-memory stand-in for tsidp's client-management
// HTTP API, faithful to the semantics the operator depends on: the plaintext
// client_secret appears only in the POST /register response, reads blank it,
// DELETE returns 204 (404 for unknown IDs), and in-place updates go through
// the /edit/{id} form endpoint. Used by unit and contract tests.
//
// Contract snapshot: verified against tsidp v0.0.15 (server/clients.go,
// server/ui.go). Re-verify whenever the pin in hack/tsidp.Dockerfile moves.
package tsidpfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Client is a registered client held by the fake.
type Client struct {
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret,omitempty"`
	ClientName   string    `json:"client_name,omitempty"`
	RedirectURIs []string  `json:"redirect_uris,omitempty"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
}

// Server is a fake tsidp.
type Server struct {
	Issuer string

	mu      sync.Mutex
	clients map[string]*Client
	nextID  int
}

// New returns a fake tsidp backed by an httptest.Server. Callers own ts.Close.
func New() (*Server, *httptest.Server) {
	s := &Server{clients: map[string]*Client{}}
	ts := httptest.NewServer(s)
	s.Issuer = ts.URL
	return s, ts
}

// Snapshot returns a copy of the current client set keyed by ID.
func (s *Server) Snapshot() map[string]Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Client, len(s.clients))
	for id, c := range s.clients {
		out[id] = *c
	}
	return out
}

// Seed inserts a client directly, as if registered earlier.
func (s *Server) Seed(c Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cc := c
	s.clients[c.ClientID] = &cc
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/register":
		s.serveRegister(w, r)
	case r.URL.Path == "/.well-known/openid-configuration":
		s.serveDiscovery(w, r)
	case r.URL.Path == "/clients/" || r.URL.Path == "/clients":
		s.serveList(w, r)
	case strings.HasPrefix(r.URL.Path, "/clients/"):
		s.serveClient(w, r, strings.TrimPrefix(r.URL.Path, "/clients/"))
	case strings.HasPrefix(r.URL.Path, "/edit/"):
		s.serveEdit(w, r, strings.TrimPrefix(r.URL.Path, "/edit/"))
	default:
		httpError(w, http.StatusNotFound, "not_found", "not found")
	}
}

func httpError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc}) //nolint:errcheck
}

func (s *Server) serveRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	if len(req.RedirectURIs) == 0 {
		httpError(w, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required")
		return
	}
	s.mu.Lock()
	s.nextID++
	c := &Client{
		ClientID:     fmt.Sprintf("fake-client-%d", s.nextID),
		ClientSecret: fmt.Sprintf("fake-secret-%d", s.nextID),
		ClientName:   req.ClientName,
		RedirectURIs: req.RedirectURIs,
		CreatedAt:    time.Now().UTC(),
	}
	s.clients[c.ClientID] = c
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(c) //nolint:errcheck
}

func (s *Server) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"issuer":                 s.Issuer,
		"authorization_endpoint": s.Issuer + "/authorize",
		"token_endpoint":         s.Issuer + "/token",
		"userinfo_endpoint":      s.Issuer + "/userinfo",
		"jwks_uri":               s.Issuer + "/.well-known/jwks.json",
	})
}

func (s *Server) serveList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	s.mu.Lock()
	out := make([]Client, 0, len(s.clients))
	for _, c := range s.clients {
		cc := *c
		cc.ClientSecret = "" // tsidp blanks secrets on list
		out = append(out, cc)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out) //nolint:errcheck
}

func (s *Server) serveClient(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	c, ok := s.clients[id]
	s.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if !ok {
			httpError(w, http.StatusNotFound, "not_found", "client not found")
			return
		}
		cc := *c
		cc.ClientSecret = "" // tsidp blanks secrets on get
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cc) //nolint:errcheck
	case http.MethodDelete:
		if !ok {
			httpError(w, http.StatusNotFound, "not_found", "client not found")
			return
		}
		s.mu.Lock()
		delete(s.clients, id)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		httpError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
	}
}

// dangerousSchemes mirrors tsidp's validateRedirectURI blocklist (ui.go),
// which the real /edit handler enforces even though /register does not.
var dangerousSchemes = []string{
	"javascript:", "data:", "file:", "vbscript:", "about:", "blob:",
	"filesystem:", "chrome:", "chrome-extension:", "ftp:", "mailto:",
}

func editRejects(uri string) bool {
	lower := strings.ToLower(uri)
	for _, s := range dangerousSchemes {
		if strings.HasPrefix(lower, s) {
			return true
		}
	}
	return !strings.Contains(uri, ":")
}

func (s *Server) serveEdit(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	if err := r.ParseForm(); err != nil {
		httpError(w, http.StatusBadRequest, "invalid_request", "failed to parse form")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[id]
	if !ok {
		httpError(w, http.StatusNotFound, "not_found", "client not found")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	var uris []string
	for _, u := range strings.Split(r.FormValue("redirect_uris"), "\n") {
		if u = strings.TrimSpace(u); u != "" {
			uris = append(uris, u)
		}
	}
	// CONTRACT-CRITICAL: the real handler reports every validation failure
	// by re-rendering the HTML form at HTTP 200 (renderFormError never
	// sets a status). Status codes cannot signal edit failure; callers
	// must verify by reading the client back.
	if len(uris) == 0 {
		fmt.Fprint(w, "<html><body>error: at least one redirect URI is required</body></html>")
		return
	}
	for _, u := range uris {
		if editRejects(u) {
			fmt.Fprintf(w, "<html><body>error: invalid redirect URI %q</body></html>", u)
			return
		}
	}
	c.ClientName = name
	c.RedirectURIs = uris
	fmt.Fprint(w, "<html><body>client updated</body></html>")
}
