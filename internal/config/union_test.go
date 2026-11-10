package config

import (
	"reflect"
	"testing"
)

// listeners is a small constructor for test-only Config values: each
// argument is one listener's already-resolved Capabilities list (as
// defaults.go would have left it — UnionCapabilities never re-resolves).
func listeners(caps ...[]string) *Config {
	cfg := &Config{Listeners: make([]Listener, len(caps))}
	for i, c := range caps {
		cfg.Listeners[i].Capabilities = c
	}
	return cfg
}

// TestUnionCapabilities covers the design doc's "Backend capability
// referent" union semantics: case-insensitive name dedupe (first spelling
// wins), SIZE collapsed to the single maximum numeric value (bare SIZE
// loses to any numeric one; all-bare stays bare; absent when no listener
// advertises it), and stable first-seen output order.
func TestUnionCapabilities(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want []string
	}{
		{
			name: "dedupes across listeners",
			cfg: listeners(
				[]string{"PIPELINING", "8BITMIME"},
				[]string{"PIPELINING", "STARTTLS"},
			),
			want: []string{"PIPELINING", "8BITMIME", "STARTTLS"},
		},
		{
			name: "SIZE collapses to the numeric max",
			cfg: listeners(
				[]string{"SIZE 100"},
				[]string{"SIZE 900"},
			),
			want: []string{"SIZE 900"},
		},
		{
			name: "bare SIZE loses to a numeric one",
			cfg: listeners(
				[]string{"SIZE"},
				[]string{"SIZE 900"},
			),
			want: []string{"SIZE 900"},
		},
		{
			name: "all-bare SIZE stays bare",
			cfg: listeners(
				[]string{"SIZE"},
				[]string{"SIZE"},
			),
			want: []string{"SIZE"},
		},
		{
			name: "no listener advertises SIZE: absent",
			cfg: listeners(
				[]string{"PIPELINING"},
				[]string{"8BITMIME"},
			),
			want: []string{"PIPELINING", "8BITMIME"},
		},
		{
			name: "case-insensitive dedupe keeps first spelling",
			cfg: listeners(
				[]string{"pipelining"},
				[]string{"PIPELINING"},
			),
			want: []string{"pipelining"},
		},
		{
			name: "single listener is identical to its own list",
			cfg:  listeners([]string{"PIPELINING", "8BITMIME", "SIZE 10485760", "STARTTLS"}),
			want: []string{"PIPELINING", "8BITMIME", "SIZE 10485760", "STARTTLS"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.UnionCapabilities()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("UnionCapabilities() = %q, want %q", got, tc.want)
			}
		})
	}
}
