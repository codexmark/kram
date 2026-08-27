package app

import "testing"

func TestParseContextWindowInput(t *testing.T) {
	cases := map[string]int{
		"":        0, // blank = unknown
		"  ":      0, // whitespace = unknown
		"32768":   32768,
		" 128000": 128000, // trimmed
		"-5":      0,      // negative clamped to unknown
		"abc":     0,      // unparseable = unknown, not a crash
		"12.5":    0,      // non-integer = unknown
	}
	for in, want := range cases {
		if got := parseContextWindowInput(in); got != want {
			t.Errorf("parseContextWindowInput(%q) = %d, want %d", in, got, want)
		}
	}
}
