package executor

import "testing"

func TestChildPath(t *testing.T) {
	cases := []struct {
		binary string
		want   string
	}{
		{"/home/pti/google-cloud-sdk/bin/kubectl", "/home/pti/google-cloud-sdk/bin:/usr/bin:/bin"},
		{"/opt/tools/gcloud", "/opt/tools:/usr/bin:/bin"},
		{"/usr/bin/git", "/usr/bin:/bin"}, // already in base, no duplicate
		{"/bin/ls", "/usr/bin:/bin"},
		{"kubectl", "/usr/bin:/bin"}, // bare name → "." → base only
	}
	for _, c := range cases {
		if got := childPath(c.binary); got != c.want {
			t.Errorf("childPath(%q) = %q, want %q", c.binary, got, c.want)
		}
	}
}
