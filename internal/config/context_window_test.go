package config

import "testing"

func TestComboContextWindowTakesMinIgnoringUnknown(t *testing.T) {
	c := &Config{
		Providers: []ProviderConfig{
			{ID: "big", ContextWindow: 200000},
			{ID: "small", ContextWindow: 32768},
			{ID: "unknown", ContextWindow: 0}, // unknown — must be ignored, not treated as the min
		},
		Combos: []ComboConfig{
			{ID: "mixed", Providers: []string{"big", "small", "unknown"}},
			{ID: "onlyBig", Providers: []string{"big"}},
			{ID: "allUnknown", Providers: []string{"unknown"}},
		},
	}

	if got := c.ComboContextWindow("mixed"); got != 32768 {
		t.Errorf("mixed combo window = %d, want 32768 (min, ignoring the unknown)", got)
	}
	if got := c.ComboContextWindow("onlyBig"); got != 200000 {
		t.Errorf("onlyBig combo window = %d, want 200000", got)
	}
	if got := c.ComboContextWindow("allUnknown"); got != 0 {
		t.Errorf("allUnknown combo window = %d, want 0 (nothing known)", got)
	}
	if got := c.ComboContextWindow("nonexistent"); got != 0 {
		t.Errorf("nonexistent combo window = %d, want 0", got)
	}
}
