package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validTestConfig() Config {
	return Config{Providers: []ProviderConfig{{ID: "p", Kind: "anthropic"}}, Combos: []ComboConfig{{ID: "c", Providers: []string{"p"}}}, DefaultCombo: "c"}
}

func TestProviderAPIKeyModes(t *testing.T) {
	t.Setenv("KRAM_TEST_KEY", "secret")
	for _, tc := range []struct {
		p       ProviderConfig
		want    string
		wantErr bool
	}{
		{p: ProviderConfig{}, want: ""},
		{p: ProviderConfig{APIKeyEnv: "KRAM_TEST_KEY"}, want: "secret"},
		{p: ProviderConfig{ID: "required", APIKeyEnv: "KRAM_TEST_MISSING"}, wantErr: true},
		{p: ProviderConfig{ID: "optional", APIKeyEnv: "KRAM_TEST_MISSING", KeyOptional: true}, want: ""},
	} {
		got, err := tc.p.APIKey()
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Fatalf("APIKey(%#v)=(%q,%v)", tc.p, got, err)
		}
	}
}

func TestValidateEveryStructuralError(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Config)
		contains string
	}{
		{"no providers", func(c *Config) { c.Providers = nil }, "at least one provider"},
		{"missing provider id", func(c *Config) { c.Providers[0].ID = "" }, "missing id"},
		{"duplicate provider", func(c *Config) { c.Providers = append(c.Providers, c.Providers[0]) }, "duplicate provider"},
		{"bad kind", func(c *Config) { c.Providers[0].Kind = "bad" }, "unknown kind"},
		{"bad auth", func(c *Config) { c.Providers[0].AuthMode = "cookie" }, "unknown auth_mode"},
		{"no combos", func(c *Config) { c.Combos = nil }, "at least one combo"},
		{"missing combo id", func(c *Config) { c.Combos[0].ID = "" }, "combo missing id"},
		{"duplicate combo", func(c *Config) { c.Combos = append(c.Combos, c.Combos[0]) }, "duplicate combo"},
		{"empty combo", func(c *Config) { c.Combos[0].Providers = nil }, "has no providers"},
		{"unknown provider", func(c *Config) { c.Combos[0].Providers = []string{"missing"} }, "unknown provider"},
		{"bad default", func(c *Config) { c.DefaultCombo = "missing" }, "default_combo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validTestConfig()
			tc.mutate(&c)
			err := c.validate()
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("err=%v want %q", err, tc.contains)
			}
		})
	}
	for _, kind := range []string{"anthropic", "gemini", "openai-compat", "openai-responses"} {
		c := validTestConfig()
		c.Providers[0].Kind = kind
		if err := c.validate(); err != nil {
			t.Fatalf("kind %s: %v", kind, err)
		}
	}
}

func TestLoadDefaultsAndSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")
	c := validTestConfig()
	c.Host = ""
	c.Port = 0
	if err := Save(&c, path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), generatedHeader) {
		t.Fatalf("missing generated header: %s", b)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "127.0.0.1" || got.Port != 20128 {
		t.Fatalf("defaults=%s:%d", got.Host, got.Port)
	}
	if err := Save(&c, path); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
}

func TestLoadAndSaveFailures(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing load should fail")
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("providers: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "parsing") {
		t.Fatalf("err=%v", err)
	}
	invalid := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("providers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(invalid); err == nil {
		t.Fatal("invalid config should fail")
	}
	parentFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(&validConfigValue, parentFile+"/child"); err == nil {
		t.Fatal("mkdir under file should fail")
	}
}

var validConfigValue = validTestConfig()
