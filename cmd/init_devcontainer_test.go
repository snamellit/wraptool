package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
	t.Run("npm harness auto-adds node and is valid JSONC", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), ".devcontainer", "devcontainer.json")
		if err := runInitDevcontainer("claude", []string{"go"}, "ubuntu", out, "", false); err != nil {
			t.Fatalf("scaffold: %v", err)
		}
		m := parseJSONC(t, out)
		keys := featureKeys(t, m)
		if !hasFeature(keys, "features/go:") {
			t.Errorf("missing go feature; got %v", keys)
		}
		if !hasFeature(keys, "features/node:") {
			t.Errorf("npm harness must auto-add node; got %v", keys)
		}
		wt, ok := m["features"].(map[string]any)["forge.snamellit.com/pti/wraptool/wraptool:0"].(map[string]any)
		if !ok || wt["harness"] != "claude" {
			t.Errorf("wraptool feature missing or wrong harness; got %v", m["features"])
		}
		if m["image"] != "mcr.microsoft.com/devcontainers/base:ubuntu" {
			t.Errorf("base image = %v", m["image"])
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

	t.Run("gemini scaffolds provision:false", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), ".devcontainer", "devcontainer.json")
		if err := runInitDevcontainer("gemini", nil, "ubuntu", out, "", false); err != nil {
			t.Fatalf("scaffold: %v", err)
		}
		wt := parseJSONC(t, out)["features"].(map[string]any)["forge.snamellit.com/pti/wraptool/wraptool:0"].(map[string]any)
		if wt["provision"] != false {
			t.Errorf("gemini must scaffold provision:false; got %v", wt)
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
