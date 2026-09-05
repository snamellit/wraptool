package cmd

import "testing"

func TestAdminLoginURL(t *testing.T) {
	cases := []struct {
		listen, want string
	}{
		{"127.0.0.1:9091", "http://127.0.0.1:9091/?token=s3cret"},
		{"localhost:9091", "http://localhost:9091/?token=s3cret"},
		{":9091", "http://127.0.0.1:9091/?token=s3cret"},
		{"0.0.0.0:9091", "http://127.0.0.1:9091/?token=s3cret"},
	}
	for _, c := range cases {
		if got := adminLoginURL(c.listen, "s3cret"); got != c.want {
			t.Errorf("adminLoginURL(%q) = %q, want %q", c.listen, got, c.want)
		}
	}
	// Token must be query-escaped.
	if got := adminLoginURL("127.0.0.1:9091", "a b&c"); got != "http://127.0.0.1:9091/?token=a+b%26c" {
		t.Errorf("escaping: got %q", got)
	}
}
