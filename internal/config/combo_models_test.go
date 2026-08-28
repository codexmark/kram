package config

import (
	"reflect"
	"testing"
)

func TestComboModels(t *testing.T) {
	cfg := &Config{
		Providers: []ProviderConfig{
			{ID: "a", Model: "claude-opus-5"},
			{ID: "b", Model: ""},
			{ID: "c", Model: "qwen2.5-coder-14b"},
		},
		Combos: []ComboConfig{
			{ID: "default", Providers: []string{"a", "b"}},
			{ID: "local", Providers: []string{"c"}},
		},
	}
	if got := cfg.ComboModels("default"); !reflect.DeepEqual(got, []string{"claude-opus-5", ""}) {
		t.Fatalf("ComboModels(default) = %v", got)
	}
	if got := cfg.ComboModels("local"); !reflect.DeepEqual(got, []string{"qwen2.5-coder-14b"}) {
		t.Fatalf("ComboModels(local) = %v", got)
	}
	if got := cfg.ComboModels("missing"); got != nil {
		t.Fatalf("ComboModels(missing) = %v, want nil", got)
	}
}
