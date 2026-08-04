package service

import "testing"

func TestValidName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"demo", true},
		{"demo-2", true},
		{"a", true},
		{"", false},
		{"Demo", false},
		{"1demo", false},
		{"-demo", false},
		{"../etc", false},
		{"demo/evil", false},
		{"demo evil", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidName(tt.name); got != tt.want {
				t.Errorf("ValidName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
