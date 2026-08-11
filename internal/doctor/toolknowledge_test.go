package doctor

import (
	"reflect"
	"testing"
)

// ---------------------------------------------------------------- LookupToolKnowledge

func TestLookupToolKnowledge(t *testing.T) {
	for _, name := range []string{"git", "kubectl", "gcloud", "terraform"} {
		t.Run(name, func(t *testing.T) {
			tk, ok := LookupToolKnowledge(name)
			if !ok {
				t.Fatalf("LookupToolKnowledge(%q) ok = false, want true", name)
			}
			if len(tk.CatalogArgv) == 0 {
				t.Errorf("LookupToolKnowledge(%q).CatalogArgv is empty, want a catalog argv", name)
			}
		})
	}

	// Case-insensitive: a caller that skips ToolBasename's own lowercasing
	// is not punished for it.
	if tk, ok := LookupToolKnowledge("KUBECTL"); !ok || tk.DangerousFlags == nil {
		t.Errorf("LookupToolKnowledge(%q) = %+v, %v, want a populated kubectl entry", "KUBECTL", tk, ok)
	}
}

// TestLookupToolKnowledgeUnknown is the bounding-rule test: a tool with no
// entry — including glab, named elsewhere in the codebase as an example
// tool basename but never given per-tool data by either design doc — gets
// ok=false, never a guessed or zero-value-but-"found" entry.
func TestLookupToolKnowledgeUnknown(t *testing.T) {
	for _, name := range []string{"glab", "docker", "aws", "", "made-up-tool-xyz"} {
		t.Run(name, func(t *testing.T) {
			tk, ok := LookupToolKnowledge(name)
			if ok {
				t.Errorf("LookupToolKnowledge(%q) ok = true, want false (no per-tool data exists)", name)
			}
			if !reflect.DeepEqual(tk, ToolKnowledge{}) {
				t.Errorf("LookupToolKnowledge(%q) = %+v, want the zero value when ok is false", name, tk)
			}
		})
	}
}

// ---------------------------------------------------------------- ToolBasename

func TestToolBasename(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/usr/bin/git", "git"},
		{"/usr/local/bin/kubectl", "kubectl"},
		{"gcloud", "gcloud"},
		{"GCLOUD", "gcloud"},
		{`C:\Program Files\Git\bin\git.exe`, "git"},
		{`C:\tools\npm\kubectl.cmd`, "kubectl"},
		{"/usr/bin/terraform.exe", "terraform"}, // exe suffix stripped even on a POSIX-shaped path
		{"", "."},                               // filepath.Base("") == "."
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			if got := ToolBasename(tc.path); got != tc.want {
				t.Errorf("ToolBasename(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- IsDangerousFlag / IsGenericDangerousFlag

func TestIsDangerousFlag(t *testing.T) {
	git, ok := LookupToolKnowledge("git")
	if !ok {
		t.Fatal("git entry missing")
	}
	for _, f := range []string{"--upload-pack", "--receive-pack", "--exec", "-c", "--config"} {
		if !git.IsDangerousFlag(f) {
			t.Errorf("git.IsDangerousFlag(%q) = false, want true", f)
		}
	}
	for _, f := range []string{"--verbose", "-v", "--Config", "config"} {
		if git.IsDangerousFlag(f) {
			t.Errorf("git.IsDangerousFlag(%q) = true, want false", f)
		}
	}

	kubectl, ok := LookupToolKnowledge("kubectl")
	if !ok {
		t.Fatal("kubectl entry missing")
	}
	// git's dangerous flags must not leak into kubectl's set.
	if kubectl.IsDangerousFlag("--upload-pack") {
		t.Errorf("kubectl.IsDangerousFlag(%q) = true, want false (that is git's flag)", "--upload-pack")
	}
	for _, f := range []string{"--kubeconfig", "--token", "--server", "--as"} {
		if !kubectl.IsDangerousFlag(f) {
			t.Errorf("kubectl.IsDangerousFlag(%q) = false, want true", f)
		}
	}
}

func TestIsGenericDangerousFlag(t *testing.T) {
	tests := []struct {
		flag string
		want bool
	}{
		{"--env-file", true},
		{"--flags-file", true},
		{"--exec-cmd", true},
		{"--pre-cmd", true},
		{"--File", true}, // case-insensitive
		{"--CMD", true},  // case-insensitive; "--CMD" ends in "-cmd" once lowercased
		{"--kubeconfig", false},
		{"--token", false},
		{"-f", false},   // short flag: no descriptive suffix to match
		{"file", false}, // not even flag-shaped (no leading "--")
		{"-cmd", false}, // single-dash: not a long flag
		{"--", false},
	}
	for _, tc := range tests {
		t.Run(tc.flag, func(t *testing.T) {
			if got := IsGenericDangerousFlag(tc.flag); got != tc.want {
				t.Errorf("IsGenericDangerousFlag(%q) = %v, want %v", tc.flag, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- ShellEquivalentMatch

func TestShellEquivalentMatch(t *testing.T) {
	kubectl, _ := LookupToolKnowledge("kubectl")
	gcloud, _ := LookupToolKnowledge("gcloud")
	terraform, _ := LookupToolKnowledge("terraform")

	tests := []struct {
		name       string
		tk         ToolKnowledge
		subcommand []string
		wantOK     bool
		wantPath   []string
	}{
		{"kubectl exec: one segment matches", kubectl, []string{"exec"}, true, []string{"exec"}},
		{"kubectl cp: one segment matches", kubectl, []string{"cp"}, true, []string{"cp"}},
		{"kubectl get: not shell-equivalent", kubectl, []string{"get"}, false, nil},
		{"gcloud compute ssh: two segments match", gcloud, []string{"compute", "ssh"}, true, []string{"compute", "ssh"}},
		{"gcloud compute instances: two segments, no match", gcloud, []string{"compute", "instances"}, false, nil},
		{"gcloud compute: too short to prefix-match", gcloud, []string{"compute"}, false, nil},
		{"gcloud compute ssh with extra trailing segment still prefix-matches", gcloud,
			[]string{"compute", "ssh", "instance-1"}, true, []string{"compute", "ssh"}},
		{"terraform has no shell-equivalent entries at all", terraform, []string{"apply"}, false, nil},
		{"empty subcommand never matches", kubectl, nil, false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.tk.ShellEquivalentMatch(tc.subcommand)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !reflect.DeepEqual(got, tc.wantPath) {
				t.Errorf("matched path = %v, want %v", got, tc.wantPath)
			}
		})
	}
}

// ---------------------------------------------------------------- MutatingVerbMatch

func TestMutatingVerbMatch(t *testing.T) {
	kubectl, _ := LookupToolKnowledge("kubectl")
	gcloud, _ := LookupToolKnowledge("gcloud")
	terraform, _ := LookupToolKnowledge("terraform")

	tests := []struct {
		name       string
		tk         ToolKnowledge
		subcommand []string
		wantVerb   string
		wantOK     bool
	}{
		{"kubectl delete: single segment matches", kubectl, []string{"delete"}, "delete", true},
		{"kubectl get: not a mutating verb", kubectl, []string{"get"}, "", false},
		// The whole reason MutatingVerbs is a LAST-segment match rather than a
		// prefix match: gcloud's verb trails an arbitrary resource path.
		{"gcloud nested resource path ending in delete matches", gcloud,
			[]string{"compute", "instances", "delete"}, "delete", true},
		{"gcloud a different nested resource path ending in delete also matches", gcloud,
			[]string{"iam", "service-accounts", "delete"}, "delete", true},
		{"gcloud set-iam-policy at depth 2 matches", gcloud,
			[]string{"projects", "set-iam-policy"}, "set-iam-policy", true},
		{"gcloud resource path NOT ending in a mutating verb", gcloud,
			[]string{"compute", "instances", "describe"}, "", false},
		{"terraform apply: single top-level segment matches", terraform, []string{"apply"}, "apply", true},
		{"terraform plan: not a mutating verb", terraform, []string{"plan"}, "", false},
		{"empty subcommand never matches", kubectl, nil, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			verb, ok := tc.tk.MutatingVerbMatch(tc.subcommand)
			if ok != tc.wantOK || verb != tc.wantVerb {
				t.Errorf("MutatingVerbMatch(%v) = (%q, %v), want (%q, %v)", tc.subcommand, verb, ok, tc.wantVerb, tc.wantOK)
			}
		})
	}
}

// ---------------------------------------------------------------- subcommandHasPrefix

func TestSubcommandHasPrefix(t *testing.T) {
	tests := []struct {
		subcommand, path []string
		want             bool
	}{
		{[]string{"exec"}, []string{"exec"}, true},
		{[]string{"exec", "-it"}, []string{"exec"}, true},
		{[]string{"compute", "ssh"}, []string{"compute", "ssh"}, true},
		{[]string{"compute"}, []string{"compute", "ssh"}, false}, // subcommand shorter than path
		{[]string{"compute", "instances"}, []string{"compute", "ssh"}, false},
		{nil, []string{"exec"}, false},
		{[]string{"exec"}, nil, true}, // an empty path is trivially a prefix of anything
	}
	for _, tc := range tests {
		if got := subcommandHasPrefix(tc.subcommand, tc.path); got != tc.want {
			t.Errorf("subcommandHasPrefix(%v, %v) = %v, want %v", tc.subcommand, tc.path, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------- stringSet

func TestStringSet(t *testing.T) {
	s := stringSet("a", "b", "a")
	if len(s) != 2 || !s["a"] || !s["b"] {
		t.Errorf("stringSet(a,b,a) = %v, want a 2-element set containing a and b", s)
	}
	empty := stringSet()
	if len(empty) != 0 {
		t.Errorf("stringSet() = %v, want empty", empty)
	}
}
