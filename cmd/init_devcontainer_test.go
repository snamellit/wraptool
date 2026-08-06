package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pti/wraptool/internal/pool"
)

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

	t.Run("non-npm harness does not add node", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), ".devcontainer", "devcontainer.json")
		if err := runInitDevcontainer("agy", nil, "ubuntu", out, "", false); err != nil {
			t.Fatalf("scaffold: %v", err)
		}
		if keys := featureKeys(t, parseJSONC(t, out)); hasFeature(keys, "features/node:") {
			t.Errorf("agy is not an npm harness; node should not be added; got %v", keys)
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

	t.Run("no --harness-version flag (removed provisioning option)", func(t *testing.T) {
		cmd := newInitDevcontainerCmd()
		cmd.SetArgs([]string{"--harness-version", "1.0.0"})
		if err := cmd.Execute(); err == nil {
			t.Error("--harness-version must not exist; the wiring-only feature has no project version option")
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
}
