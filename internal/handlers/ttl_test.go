package handlers

import "testing"

func TestParseTTLText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"days", "30j", "720h0m0s"},
		{"hours", "12h", "12h0m0s"},
		{"minutes", "90m", "0h90m0s"},
		{"uppercase suffix", "5H", "5h0m0s"},
		{"leading/trailing spaces", "  7j  ", "168h0m0s"},
		{"empty means use the configured default", "", ""},
		{"no suffix is invalid", "30", ""},
		{"unknown suffix is invalid", "30x", ""},
		{"zero is invalid", "0j", ""},
		{"negative is invalid", "-5j", ""},
		{"not a number at all", "abcj", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseTTLText(tt.input); got != tt.want {
				t.Errorf("parseTTLText(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTTLToText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"days", "720h0m0s", "30j"},
		{"hours", "12h0m0s", "12h"},
		{"minutes", "0h90m0s", "90m"},
		{"empty", "", ""},
		{"unparsable falls back to the raw string", "not-a-duration", "not-a-duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ttlToText(tt.input); got != tt.want {
				t.Errorf("ttlToText(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
