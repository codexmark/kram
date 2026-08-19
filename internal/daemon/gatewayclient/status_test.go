package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatusDecodesProvidersAndCombos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/status" {
			t.Errorf("request path = %q, want /admin/status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Status{
			Providers: []ProviderStatus{{ID: "p1", SupportsImages: true, SupportsTools: true}},
			Combos:    []ComboStatus{{ID: "default", Strategy: "round-robin", Providers: []string{"p1"}}},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	status, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Providers) != 1 || status.Providers[0].ID != "p1" {
		t.Errorf("Providers = %+v, want one entry for p1", status.Providers)
	}
	if len(status.Combos) != 1 || status.Combos[0].ID != "default" {
		t.Errorf("Combos = %+v, want one entry for default", status.Combos)
	}
}

func TestStatusReturnsErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL)
	if _, err := c.Status(context.Background()); err == nil {
		t.Error("expected an error for a 500 status response")
	}
}

func TestStatusReturnsErrorOnMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	c := New(srv.URL)
	if _, err := c.Status(context.Background()); err == nil {
		t.Error("expected a decode error for malformed JSON")
	}
}

func TestComboSupportsImagesTrueWhenAnyMemberDoes(t *testing.T) {
	status := Status{
		Providers: []ProviderStatus{
			{ID: "text-only", SupportsImages: false},
			{ID: "vision", SupportsImages: true},
		},
		Combos: []ComboStatus{{ID: "default", Providers: []string{"text-only", "vision"}}},
	}
	if !status.ComboSupportsImages("default") {
		t.Error("expected true: combo has a member that supports images")
	}
}

func TestComboSupportsImagesFalseWhenNoMemberDoes(t *testing.T) {
	status := Status{
		Providers: []ProviderStatus{{ID: "text-only", SupportsImages: false}},
		Combos:    []ComboStatus{{ID: "default", Providers: []string{"text-only"}}},
	}
	if status.ComboSupportsImages("default") {
		t.Error("expected false: no member of the combo supports images")
	}
}

func TestComboSupportsImagesFalseForUnknownCombo(t *testing.T) {
	status := Status{}
	if status.ComboSupportsImages("does-not-exist") {
		t.Error("expected false for a combo ID that doesn't exist in Status")
	}
}
