package proxy

import "testing"

// TestMailFromDomain is mailFromDomain's own direct coverage: the R4
// byte-exactness for these inputs (nothing rewritten on the wire in
// either direction) is proven separately, end to end, by
// TestMailFromMalformedAddressVerbatim and TestMailFromNullSenderVerbatim
// in relay_test.go. This table only checks the routing key it derives.
func TestMailFromDomain(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "well-formed with params",
			line: "MAIL FROM:<user@example.com> SIZE=100\r\n",
			want: "example.com",
		},
		{
			name: "null sender",
			line: "MAIL FROM:<>\r\n",
			want: "",
		},
		{
			name: "garbage, no @ or brackets",
			line: "MAIL FROM:not-an-address-at-all\r\n",
			want: "",
		},
		{
			name: "mixed-case domain is lowered",
			line: "MAIL FROM:<USER@EXAMPLE.COM>\r\n",
			want: "example.com",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mailFromDomain([]byte(tc.line)); got != tc.want {
				t.Errorf("mailFromDomain(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}
