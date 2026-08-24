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
			Providers:  []Provider{{ID: "p1", Kind: "anthropic", BreakerOpen: false, Stats: ProviderStats{Requests: 10}}},
			Combos:     []Combo{{ID: "default", Strategy: "smart", Providers: []string{"p1"}}},
			Strategies: []string{"priority", "smart"},
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
	if len(status.Strategies) != 2 || status.Strategies[1] != "smart" {
		t.Errorf("Strategies = %+v", status.Strategies)
	}
}

func TestSetStrategySendsMutationAndDecodesCombo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/strategy" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["combo"] != "default" || body["strategy"] != "round-robin" {
			t.Fatalf("body=%v", body)
		}
		_ = json.NewEncoder(w).Encode(Combo{ID: "default", Strategy: "round-robin", Providers: []string{"a", "b"}})
	}))
	defer srv.Close()

	updated, err := New(srv.URL).SetStrategy(context.Background(), "default", "round-robin")
	if err != nil || updated.Strategy != "round-robin" {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
}

func TestSetStrategyReturnsGatewayErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"unknown strategy"}}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL).SetStrategy(context.Background(), "default", "bad"); err == nil || err.Error() != "gateway: unknown strategy" {
		t.Fatalf("err=%v", err)
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
