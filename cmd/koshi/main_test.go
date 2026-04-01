package main

import "testing"

func TestResolveListenAddr(t *testing.T) {
	tests := []struct {
		name     string
		cfgAddr  string
		envAddr  string
		expected string
	}{
		{"env override wins", ":15080", ":16080", ":16080"},
		{"config used when env empty", ":15080", "", ":15080"},
		{"fallback when both empty", "", "", ":8080"},
		{"env wins over default", "", ":9090", ":9090"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveListenAddr(tt.cfgAddr, tt.envAddr)
			if got != tt.expected {
				t.Errorf("resolveListenAddr(%q, %q) = %q, want %q",
					tt.cfgAddr, tt.envAddr, got, tt.expected)
			}
		})
	}
}
