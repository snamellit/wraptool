package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/adrg/xdg"
	"github.com/pti/wraptool/internal/pool"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it — used to assert on runInitDevcontainer's printed
// next-steps, which aren't otherwise observable (they go straight to
// os.Stdout, not a cobra OutOrStdout writer).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// parseJSONC strips // line comments and parses the result, asserting the
// scaffolded config is valid JWCC (JSON-with-comments). Safe here because the
// generated file never contains "//" inside a string value.
func parseJSONC(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	re := regexp.MustCompile(`(?m)\s*//.*$`)
	stripped := re.ReplaceAllString(string(raw), "")
	var m map[string]any
	if err := json.Unmarshal([]byte(stripped), &m); err != nil {
		t.Fatalf("generated config is not valid JSONC: %v\n---\n%s", err, raw)
	}
	return m
}

func featureKeys(t *testing.T, m map[string]any) []string {
	t.Helper()
	f, ok := m["features"].(map[string]any)
	if !ok {
		t.Fatalf("no features object; got %v", m["features"])
	}
	var out []string
	for k := range f {
		out = append(out, k)
	}
	return out
}

func hasFeature(keys []string, substr string) bool {
	for _, k := range keys {
		if strings.Contains(k, substr) {
			return true
		}
	}
	return false
}

func TestInitDevcontainer(t *testing.T) {
	t.Run("npm harness no longer auto-adds node; valid JSONC", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), ".devcontainer", "devcontainer.json")
		if err := runInitDevcontainer("claude", []string{"go"}, "ubuntu", out, "", false); err != nil {
			t.Fatalf("scaffold: %v", err)
		}
		m := parseJSONC(t, out)
		keys := featureKeys(t, m)
		if !hasFeature(keys, "features/go:") {
			t.Errorf("missing go feature; got %v", keys)
		}
		if hasFeature(keys, "features/node:") {
			t.Errorf("node must not be auto-added any more (pool runtime bundles its own); got %v", keys)
		}
		wt, ok := m["features"].(map[string]any)["forge.snamellit.com/pti/wraptool/wraptool:1"].(map[string]any)
		if !ok || wt["harness"] != "claude" {
			t.Errorf("wraptool feature missing or wrong harness; got %v", m["features"])
		}
		if len(wt) != 1 {
			t.Errorf("wraptool feature must carry only 'harness' (no provision/version); got %v", wt)
		}
		if m["image"] != "mcr.microsoft.com/devcontainers/base:ubuntu" {
			t.Errorf("base image = %v", m["image"])
		}
	})

	t.Run("feature ref is the wiring-only major (:1)", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), ".devcontainer", "devcontainer.json")
		if err := runInitDevcontainer("claude", nil, "ubuntu", out, "", false); err != nil {
			t.Fatalf("scaffold: %v", err)
		}
		keys := featureKeys(t, parseJSONC(t, out))
		if !hasFeature(keys, "wraptool/wraptool:1") {
			t.Errorf("expected the :1 wiring-only feature ref; got %v", keys)
		}
		if hasFeature(keys, "wraptool/wraptool:0") {
			t.Errorf("must not scaffold the legacy :0 provisioning major; got %v", keys)
		}
	})

	t.Run("no harness version or provision option scaffolded", func(t *testing.T) {
		// §16 Phase 4 item 2: the project version option and any provision
		// toggle are gone from the wiring-only feature schema, and the
		// scaffolder must not emit them for any harness, including gemini
		// (which used to get an explicit "provision": false).
		for _, h := range []string{"claude", "opencode", "pi", "agy", "gemini"} {
			out := filepath.Join(t.TempDir(), ".devcontainer", "devcontainer.json")
			if err := runInitDevcontainer(h, nil, "ubuntu", out, "", false); err != nil {
				t.Fatalf("scaffold %s: %v", h, err)
			}
			wt, ok := parseJSONC(t, out)["features"].(map[string]any)["forge.snamellit.com/pti/wraptool/wraptool:1"].(map[string]any)
			if !ok {
				t.Fatalf("%s: wraptool feature missing", h)
			}
			if _, has := wt["provision"]; has {
				t.Errorf("%s: scaffold must not emit a provision option; got %v", h, wt)
			}
			if _, has := wt["version"]; has {
				t.Errorf("%s: scaffold must not emit a version option; got %v", h, wt)
			}
		}
	})

	t.Run("both pool mounts emitted with correct ro/rw and ${localEnv:HOME} rewrite", func(t *testing.T) {
		// The bind sources must match where the pool actually lives
		// (paths.DataDir() via internal/pool), not a hardcoded ~/.local/share
		// — which only holds on Linux and breaks the mount on macOS
		// (~/Library/Application Support).
		out := filepath.Join(t.TempDir(), ".devcontainer", "devcontainer.json")
		if err := runInitDevcontainer("claude", nil, "ubuntu", out, "", false); err != nil {
			t.Fatalf("scaffold: %v", err)
		}
		mounts, ok := parseJSONC(t, out)["mounts"].([]any)
		if !ok || len(mounts) != 2 {
			t.Fatalf("expected two mounts (home + runtime); got %v", parseJSONC(t, out)["mounts"])
		}

		// Entry 0: home, object form, read-write (no readonly field expressible
		// in object form anyway, but the point is it must not claim readonly).
		home, ok := mounts[0].(map[string]any)
		if !ok {
			t.Fatalf("home mount must be object form {source,target,type}; got %T %v", mounts[0], mounts[0])
		}
		if home["source"] != poolHomeMountSource() {
			t.Errorf("home mount source = %v, want %q", home["source"], poolHomeMountSource())
		}
		if home["target"] != devcontainerPoolHome || home["type"] != "bind" {
			t.Errorf("home mount target/type = %v/%v, want %q/bind", home["target"], home["type"], devcontainerPoolHome)
		}

		// Entry 1: runtime, MUST be the verbatim string form ending in
		// ",readonly" — object-form mounts silently drop read-only enforcement
		// (design/harness/unified-harness-pool.md §12.2's CLI-source-proven
		// ruling; see cmd/up.go's devcontainerClassifyMount).
		runtimeEntry, ok := mounts[1].(string)
		if !ok {
			t.Fatalf("runtime mount must be STRING form (object form can't express read-only); got %T %v", mounts[1], mounts[1])
		}
		if !strings.HasSuffix(runtimeEntry, ",readonly") {
			t.Errorf("runtime mount must end in ,readonly; got %q", runtimeEntry)
		}
		if !strings.Contains(runtimeEntry, "target="+devcontainerPoolRuntime) {
			t.Errorf("runtime mount target wrong; got %q, want target=%s", runtimeEntry, devcontainerPoolRuntime)
		}
		if !strings.Contains(runtimeEntry, "type=bind") {
			t.Errorf("runtime mount must be type=bind; got %q", runtimeEntry)
		}
		wantSrc, _ := poolMountSourceOf(pool.RuntimeDir())
		if !strings.Contains(runtimeEntry, "source="+wantSrc+",") {
			t.Errorf("runtime mount source wrong; got %q, want source=%s", runtimeEntry, wantSrc)
		}

		// End-to-end proof of the CRITICAL FORM RULING: `up`'s own
		// devcontainerClassifyMount (cmd/up.go) must read this scaffolded
		// entry back as genuinely read-only, not merely "looks right" by
		// string inspection above.
		root := filepath.Dir(filepath.Dir(out)) // .../.devcontainer/devcontainer.json -> project root
		if got := devcontainerClassifyMount(root, devcontainerPoolRuntime); got != devcontainerMountReadOnly {
			t.Errorf("devcontainerClassifyMount(scaffolded runtime mount) = %v, want devcontainerMountReadOnly", got)
		}

		// On any host whose data dir is under $HOME, both sources must use the
		// client-expanded ${localEnv:HOME}, never a baked-in absolute home path.
		// ToSlash the home side too: src is forward-slashed, so a Windows
		// C:\Users\... home would never substring-match without it. Skip home=="/"
		// (minimal containers): every path contains "/".
		if hostHome, err := os.UserHomeDir(); err == nil && hostHome != "/" {
			hostHome = filepath.ToSlash(hostHome)
			if src, _ := home["source"].(string); strings.Contains(src, hostHome) {
				t.Errorf("home mount source leaks absolute home %q: %q", hostHome, src)
			}
			if strings.Contains(runtimeEntry, hostHome) {
				t.Errorf("runtime mount source leaks absolute home %q: %q", hostHome, runtimeEntry)
			}
		}
	})

	t.Run("data dir outside $HOME renders an absolute source and the mixed-OS caveat comment", func(t *testing.T) {
		// Force poolMountSourceOf's homeRelative==false branch deterministically:
		// os.UserHomeDir() reads $HOME live (no caching), and xdg.DataHome is a
		// plain package var (no test seam of its own in internal/paths), so both
		// are safe to override for the duration of this test.
		t.Setenv("HOME", filepath.Join(t.TempDir(), "fake-home"))
		origDataHome := xdg.DataHome
		outsideHome := filepath.Join(t.TempDir(), "elsewhere", "xdg-data")
		xdg.DataHome = outsideHome
		t.Cleanup(func() { xdg.DataHome = origDataHome })

		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("UserHomeDir: %v", err)
		}
		if rel, relErr := filepath.Rel(home, pool.HomeDir()); relErr == nil && !strings.HasPrefix(rel, "..") {
			t.Fatalf("test setup broken: pool.HomeDir() %q is still under fake HOME %q", pool.HomeDir(), home)
		}

		out := filepath.Join(t.TempDir(), ".devcontainer", "devcontainer.json")
		if err := runInitDevcontainer("claude", nil, "ubuntu", out, "", false); err != nil {
			t.Fatalf("scaffold: %v", err)
		}

		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "NOTE: this host's harness-pool data dir is not under $HOME") {
			t.Errorf("expected the mixed-OS caveat comment when the data dir is outside $HOME; got:\n%s", raw)
		}

		mounts, ok := parseJSONC(t, out)["mounts"].([]any)
		if !ok || len(mounts) != 2 {
			t.Fatalf("expected two mounts; got %v", parseJSONC(t, out)["mounts"])
		}
		wantHomeSrc := filepath.ToSlash(pool.HomeDir())
		homeMount := mounts[0].(map[string]any)
		if homeMount["source"] != wantHomeSrc {
			t.Errorf("home mount source = %v, want literal absolute path %q", homeMount["source"], wantHomeSrc)
		}
		if strings.Contains(homeMount["source"].(string), "${localEnv:HOME}") {
			t.Errorf("home mount source must not use ${localEnv:HOME} when the data dir is outside $HOME: %v", homeMount["source"])
		}
		runtimeEntry := mounts[1].(string)
		wantRuntimeSrc := filepath.ToSlash(pool.RuntimeDir())
		if !strings.Contains(runtimeEntry, "source="+wantRuntimeSrc+",") {
			t.Errorf("runtime mount source = %q, want literal absolute path %q", runtimeEntry, wantRuntimeSrc)
		}
		if strings.Contains(runtimeEntry, "${localEnv:HOME}") {
			t.Errorf("runtime mount source must not use ${localEnv:HOME} when the data dir is outside $HOME: %q", runtimeEntry)
		}
	})

	t.Run("gemini next-steps and comment point at baking in the binary, not a dead-end install", func(t *testing.T) {
		// gemini has no PoolPackage/PoolArchive (harness.Registry) — it is not
		// pool-lockable, so `wraptool harness install gemini` is a dead end, and
		// "install it by hand inside the container" is equally wrong: with
		// connect:true (this scaffold's default) connect.sh hard-fails postCreate
		// before a hand-install could ever happen.
		out := filepath.Join(t.TempDir(), ".devcontainer", "devcontainer.json")
		stdout := captureStdout(t, func() {
			if err := runInitDevcontainer("gemini", nil, "ubuntu", out, "", false); err != nil {
				t.Fatalf("scaffold: %v", err)
			}
		})
		if strings.Contains(stdout, "harness install gemini") {
			t.Errorf("gemini is not pool-lockable; must not suggest `wraptool harness install gemini`; got:\n%s", stdout)
		}
		if !strings.Contains(stdout, "not pool-lockable") {
			t.Errorf("expected the printed next-steps to say gemini is not pool-lockable; got:\n%s", stdout)
		}

		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "install it by hand inside the container") {
			t.Errorf("comment must not suggest a hand-install inside the container (postCreate hard-fails first); got:\n%s", raw)
		}
		if !strings.Contains(string(raw), "not pool-lockable") {
			t.Errorf("expected the generated comment to say gemini is not pool-lockable; got:\n%s", raw)
		}
	})

	t.Run("overwrite guard", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), ".devcontainer", "devcontainer.json")
		if err := runInitDevcontainer("claude", nil, "ubuntu", out, "", false); err != nil {
			t.Fatalf("first write: %v", err)
		}
		if err := runInitDevcontainer("claude", nil, "ubuntu", out, "", false); err == nil {
			t.Error("second write without --force must error")
		}
		if err := runInitDevcontainer("claude", nil, "ubuntu", out, "", true); err != nil {
			t.Errorf("--force must overwrite: %v", err)
		}
	})

	t.Run("rejects host IDE and unknown language", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "dc.json")
		if err := runInitDevcontainer("cursor", nil, "ubuntu", out, "", false); err == nil {
			t.Error("cursor (host IDE) must be rejected")
		}
		if err := runInitDevcontainer("claude", []string{"cobol"}, "ubuntu", out, "", false); err == nil {
			t.Error("unknown language must be rejected")
		}
	})

	t.Run("a comma in the pool runtime path refuses the scaffold end to end", func(t *testing.T) {
		// A comma in the runtime source would be misread as a CSV field
		// separator by the devcontainer CLI's own mount-string parser,
		// silently corrupting the ONLY read-only-capable mount form (§12.3).
		// Force it via the same xdg.DataHome seam the branch-coverage test
		// above uses, proving the refusal end to end through
		// runInitDevcontainer/renderDevcontainer, not just the isolated helper.
		origDataHome := xdg.DataHome
		xdg.DataHome = filepath.Join(t.TempDir(), "xdg,data")
		t.Cleanup(func() { xdg.DataHome = origDataHome })

		out := filepath.Join(t.TempDir(), ".devcontainer", "devcontainer.json")
		err := runInitDevcontainer("claude", nil, "ubuntu", out, "", false)
		if err == nil {
			t.Fatal("expected an error; a comma in the pool path would corrupt the runtime mount's CSV string form")
		}
		if !strings.Contains(err.Error(), "XDG_DATA_HOME") {
			t.Errorf("error should suggest relocating XDG_DATA_HOME, got: %v", err)
		}
		if !strings.Contains(err.Error(), pool.RuntimeDir()) {
			t.Errorf("error should name the offending path %q, got: %v", pool.RuntimeDir(), err)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			t.Error("must not write a corrupted devcontainer.json on this failure")
		}
	})
}

// TestPoolRuntimeMountString mutation-checks the exact guard: every one of
// the three unsafe characters (comma, quote, backslash) individually trips
// it, a source with none of them builds successfully, and the happy path's
// output is the exact expected CSV string (so a mutant that always errors,
// or one that never does, both fail this table).
func TestPoolRuntimeMountString(t *testing.T) {
	safe := "/home/dev/.local/share/wraptool/harness-pool/runtime"
	got, err := poolRuntimeMountString(safe)
	if err != nil {
		t.Fatalf("safe path must not error: %v", err)
	}
	want := "source=" + safe + ",target=" + devcontainerPoolRuntime + ",type=bind,readonly"
	if got != want {
		t.Errorf("poolRuntimeMountString(%q) = %q, want %q", safe, got, want)
	}

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"comma", "/home/dev/xdg,data/wraptool/harness-pool/runtime"},
		{"quote", `/home/dev/xdg"data/wraptool/harness-pool/runtime`},
		{"backslash", `/home/dev/xdg\data/wraptool/harness-pool/runtime`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := poolRuntimeMountString(tc.src)
			if err == nil {
				t.Fatalf("expected an error for a source containing a %s", tc.name)
			}
			// The error message renders the offending path via %q (Go/JSON
			// escaping), so compare against the quoted form, not the raw string
			// — a literal quote or backslash in src would never substring-match
			// its own escaped rendering otherwise.
			if wantQuoted := fmt.Sprintf("%q", tc.src); !strings.Contains(err.Error(), wantQuoted) {
				t.Errorf("error should name the offending path %s, got: %v", wantQuoted, err)
			}
			if !strings.Contains(err.Error(), "XDG_DATA_HOME") {
				t.Errorf("error should suggest relocating XDG_DATA_HOME, got: %v", err)
			}
		})
	}
}
