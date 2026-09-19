package tsidp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/isvaldi-consulting/tsidp-operator/internal/tsidp"
	"github.com/isvaldi-consulting/tsidp-operator/internal/tsidp/tsidpfake"
)

// These tests double as the contract suite: the fake mirrors the semantics
// verified against tsidp's source (secret only in the 201, blanked reads,
// 204 delete, form-based edit).

func newClient(t *testing.T) (*tsidp.Client, *tsidpfake.Server) {
	t.Helper()
	fake, ts := tsidpfake.New()
	t.Cleanup(ts.Close)
	return &tsidp.Client{BaseURL: ts.URL, HTTPClient: ts.Client()}, fake
}

func TestRegisterReturnsSecretExactlyOnce(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()

	fc, err := c.Register(ctx, tsidp.RegistrationRequest{
		RedirectURIs: []string{"https://app.example.ts.net/callback"},
		ClientName:   "app",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if fc.ClientID == "" || fc.ClientSecret == "" {
		t.Fatalf("register response missing credentials: %+v", fc)
	}

	got, err := c.GetClient(ctx, fc.ClientID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	if got.ClientSecret != "" {
		t.Fatal("GetClient returned a secret; tsidp blanks it — fake is out of contract")
	}
	list, err := c.ListClients(ctx)
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}
	if len(list) != 1 || list[0].ClientSecret != "" {
		t.Fatalf("ListClients: want 1 client with blanked secret, got %+v", list)
	}
}

func TestRegisterRequiresRedirectURIs(t *testing.T) {
	c, _ := newClient(t)
	if _, err := c.Register(context.Background(), tsidp.RegistrationRequest{ClientName: "x"}); err == nil {
		t.Fatal("Register without redirect_uris should fail")
	}
}

func TestDeleteClient(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	fc, err := c.Register(ctx, tsidp.RegistrationRequest{RedirectURIs: []string{"https://a/cb"}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := c.DeleteClient(ctx, fc.ClientID); err != nil {
		t.Fatalf("DeleteClient: %v", err)
	}
	if err := c.DeleteClient(ctx, fc.ClientID); !errors.Is(err, tsidp.ErrNotFound) {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}
	if _, err := c.GetClient(ctx, fc.ClientID); !errors.Is(err, tsidp.ErrNotFound) {
		t.Fatalf("get after delete: want ErrNotFound, got %v", err)
	}
}

func TestUpdateClientInPlace(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	fc, err := c.Register(ctx, tsidp.RegistrationRequest{RedirectURIs: []string{"https://a/cb"}, ClientName: "old"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := c.UpdateClient(ctx, fc.ClientID, "new name", []string{"https://a/cb", "https://b/cb"}); err != nil {
		t.Fatalf("UpdateClient: %v", err)
	}
	got, err := c.GetClient(ctx, fc.ClientID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	if got.ClientName != "new name" || len(got.RedirectURIs) != 2 {
		t.Fatalf("update not applied: %+v", got)
	}
}

func TestDiscovery(t *testing.T) {
	c, fake := newClient(t)
	pm, err := c.Discovery(context.Background())
	if err != nil {
		t.Fatalf("Discovery: %v", err)
	}
	if pm.Issuer != fake.Issuer || pm.TokenEndpoint == "" || pm.JWKSURI == "" {
		t.Fatalf("unexpected discovery document: %+v", pm)
	}
}
