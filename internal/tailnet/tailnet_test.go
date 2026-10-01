package tailnet

import "testing"

func TestQualifyURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://gauger-server:4318", "http://gauger-server.tail1234.ts.net:4318"},
		{"http://gauger-server", "http://gauger-server.tail1234.ts.net"},
		{"http://gauger-server.tail1234.ts.net:4318", "http://gauger-server.tail1234.ts.net:4318"},
		{"http://100.64.0.7:4318", "http://100.64.0.7:4318"},
		{"http://[fd7a:115c:a1e0::7]:4318", "http://[fd7a:115c:a1e0::7]:4318"},
	} {
		got, err := QualifyURL(tc.in, "tail1234.ts.net.")
		if err != nil || got != tc.want {
			t.Errorf("QualifyURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}
