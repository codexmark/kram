package statusclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchDecodesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/status" {
			t.Errorf("request path = %q, want /admin/status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Status{
			Providers: []Provider{{ID: "p1", Kind: "anthropic", BreakerOpen: false, Stats: ProviderStats{Requests: 10}}},
			Combos:    []Combo{{ID: "default", Strategy: "smart", Providers: []string{"p1"}}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	status, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Providers) != 1 || status.Providers[0].Stats.Requests != 10 {
		t.Errorf("Providers = %+v, want one entry with Requests=10", status.Providers)
	}
	if len(status.Combos) != 1 || status.Combos[0].ID != "default" {
		t.Errorf("Combos = %+v, want one entry for default", status.Combos)
	}
}

func TestFetchReturnsErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(srv.URL)
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Error("expected an error for a 503 status response")
	}
}

func TestFetchReturnsErrorOnMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	c := New(srv.URL)
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Error("expected a decode error for malformed JSON")
	}
}

func TestFetchReturnsErrorWhenGatewayUnreachable(t *testing.T) {
	c := New("http://127.0.0.1:1") // nothing listens here
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Error("expected an error when the gateway is unreachable")
	}
}
