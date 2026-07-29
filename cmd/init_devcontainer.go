package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pti/wraptool/internal/harness"
	"github.com/pti/wraptool/internal/paths"

	"github.com/spf13/cobra"
)

// wraptoolFeatureRef is the merged wraptool Dev Container feature (provision +
// connect). Pinned to the major so patch/minor updates flow in.
const wraptoolFeatureRef = "forge.snamellit.com/pti/wraptool/wraptool:0"

// languageFeatures maps a --lang token to its devcontainers/features OCI ref, in
// canonical emit order. Aligns with docs/devcontainers.qmd (base image + features).
var languageFeatures = []struct{ name, ref string }{
	{"go", "ghcr.io/devcontainers/features/go:1"},
	{"node", "ghcr.io/devcontainers/features/node:1"},
	{"java", "ghcr.io/devcontainers/features/java:1"},
	{"python", "ghcr.io/devcontainers/features/python:1"},
	{"rust", "ghcr.io/devcontainers/features/rust:1"},
}

func languageFeatureRef(name string) (string, bool) {
	for _, l := range languageFeatures {
		if l.name == name {
			return l.ref, true
		}
	}
	return "", false
}

// npmHarnesses are provisioned via npm and so need a node runtime in the image;
// the scaffolder auto-adds the node feature for them (else the build fails when
// wraptool's install.sh finds no npm — the footgun we hit in testing).
var npmHarnesses = map[string]bool{"claude": true, "opencode": true, "pi": true}

// baseImage resolves the --base shorthand to a full image ref. A value that
// looks like a full ref (contains a slash) is passed through verbatim.
func baseImage(base string) string {
	switch base {
	case "", "ubuntu":
		return "mcr.microsoft.com/devcontainers/base:ubuntu"
	case "debian":
		return "mcr.microsoft.com/devcontainers/base:debian"
	case "alpine":
		return "mcr.microsoft.com/devcontainers/base:alpine"
	default:
		return base
	}
}

func newInitDevcontainerCmd() *cobra.Command {
	var (
		harnessName string
		langs       []string
		base        string
		out         string
		name        string
		force       bool
	)

	cmd := &cobra.Command{
		Use:   "devcontainer",
		Short: "Scaffold a .devcontainer/devcontainer.json wired for wraptool",
		Long: `Generate a starting-point Dev Container config: a base image plus the
language features you pick, the wraptool feature with a chosen harness, and the
WRAPTOOL_* env / shared-pool mount prefilled. Follows docs/devcontainers.qmd
(base image + features, not a fat all-in-one image).

It writes a NEW file and refuses to clobber an existing one (use --force). npm
harnesses (claude/opencode/pi) auto-pull the node feature so the build won't fail
for lack of npm.

Examples:
  # Go + Java project running Claude Code
  wraptool init devcontainer --harness claude --lang go,java

  # Node project running opencode (node feature added automatically anyway)
  wraptool init devcontainer --harness opencode --lang node`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInitDevcontainer(harnessName, langs, base, out, name, force)
		},
	}

	cmd.Flags().StringVar(&harnessName, "harness", "claude", "container harness to provision + wire (claude, opencode, pi, agy, gemini)")
	cmd.Flags().StringSliceVar(&langs, "lang", nil, "language features to add (go, node, java, python, rust)")
	cmd.Flags().StringVar(&base, "base", "ubuntu", "base image: ubuntu, debian, alpine, or a full image ref")
	cmd.Flags().StringVar(&out, "out", ".devcontainer/devcontainer.json", "output path")
	cmd.Flags().StringVar(&name, "name", "", "dev container name (default: workspace directory name)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config")

	return cmd
}

func runInitDevcontainer(harnessName string, langs []string, base, out, name string, force bool) error {
	// Validate the harness: must be a container-CLI harness (the wraptool feature
	// installs a binary in the box; host IDEs like cursor/roo are configured
	// host-side with `wraptool init --harness`).
	d, ok := harness.Registry[harnessName]
	if !ok || !d.IsCLI() {
		return fmt.Errorf("harness %q is not a container-CLI harness; pick one of claude, opencode, pi, agy, gemini "+
			"(host IDEs are configured with `wraptool init --harness %s`)", harnessName, harnessName)
	}

	// Resolve + de-dup the language set, auto-adding node for npm harnesses.
	want := map[string]bool{}
	var order []string
	add := func(l string) {
		if !want[l] {
			want[l] = true
			order = append(order, l)
		}
	}
	for _, l := range langs {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if _, ok := languageFeatureRef(l); !ok {
			return fmt.Errorf("unknown language %q; supported: go, node, java, python, rust", l)
		}
		add(l)
	}
	autoNode := npmHarnesses[harnessName] && !want["node"]
	if autoNode {
		add("node")
	}
	// Emit languages in canonical order regardless of input order.
	sort.SliceStable(order, func(i, j int) bool { return langIndex(order[i]) < langIndex(order[j]) })

	if name == "" {
		name = deriveName(out)
	}

	content := renderDevcontainer(name, baseImage(base), order, harnessName, autoNode)

	// Guard: never clobber a hand-authored config unless forced.
	out = expandTilde(out)
	if !force {
		if _, err := os.Stat(out); err == nil {
			return fmt.Errorf("%s already exists; pass --force to overwrite (scaffolding writes a fresh file, it does not merge)", out)
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(out), err)
	}
	if err := os.WriteFile(out, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}

	fmt.Printf("wrote %s\n", out)
	fmt.Printf("  base: %s\n", baseImage(base))
	if len(order) > 0 {
		note := strings.Join(order, ", ")
		if autoNode {
			note += " (node added for the npm harness)"
		}
		fmt.Printf("  languages: %s\n", note)
	}
	fmt.Printf("  harness: %s\n", harnessName)
	fmt.Printf("\nNext: export WRAPTOOL_TOKEN on the host if your server uses auth, then open the folder\n")
	fmt.Printf("in a Dev Container (or run `wraptool up`).\n")
	return nil
}

func langIndex(name string) int {
	for i, l := range languageFeatures {
		if l.name == name {
			return i
		}
	}
	return len(languageFeatures)
}

// deriveName picks a dev container name from the output path's project dir: the
// basename of the dir that holds .devcontainer (or the cwd).
func deriveName(out string) string {
	dir := filepath.Dir(expandTilde(out)) // .../.devcontainer
	if filepath.Base(dir) == ".devcontainer" {
		dir = filepath.Dir(dir)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if b := filepath.Base(dir); b != "" && b != "." && b != string(filepath.Separator) {
		return b
	}
	return "wraptool-devcontainer"
}

// renderDevcontainer builds the JSONC config as a commented starting point. Hand-
// built (not encoding/json) so the guiding comments survive — this is a template
// for a human to edit, not machine state.
func renderDevcontainer(name, image string, langs []string, harnessName string, autoNode bool) string {
	var b strings.Builder
	b.WriteString("{\n")
	b.WriteString("  // Generated by `wraptool init devcontainer` — a starting point; edit freely.\n")
	fmt.Fprintf(&b, "  %q: %q,\n", "name", name)
	fmt.Fprintf(&b, "  %q: %q,\n", "image", image)
	b.WriteString("\n  \"features\": {\n")
	for _, l := range langs {
		ref, _ := languageFeatureRef(l)
		fmt.Fprintf(&b, "    %q: {},", ref)
		if l == "node" && autoNode {
			b.WriteString(" // required by the npm-installed harness")
		}
		b.WriteString("\n")
	}
	// The wraptool feature. gemini has no automated installer, so scaffold it with
	// provision:false and a pointer to bake the binary in.
	b.WriteString("    // wraptool: provision installs the harness binary, connect writes the MCP wiring.\n")
	if harnessName == "gemini" {
		b.WriteString("    // gemini has no automated installer — provision is off; bake the binary into your image.\n")
		fmt.Fprintf(&b, "    %q: { \"harness\": %q, \"provision\": false }\n", wraptoolFeatureRef, harnessName)
	} else {
		fmt.Fprintf(&b, "    %q: { \"harness\": %q }\n", wraptoolFeatureRef, harnessName)
	}
	b.WriteString("  },\n")

	b.WriteString("\n  // WRAPTOOL_CWD scopes host tools to this worktree. WRAPTOOL_TOKEN authenticates\n")
	b.WriteString("  // to an auth-enabled server — kept out of the image via remoteEnv. Export it on the\n")
	b.WriteString("  // host first: export WRAPTOOL_TOKEN=$(cat ~/.config/wraptool/auth_token)\n")
	b.WriteString("  \"containerEnv\": { \"WRAPTOOL_CWD\": \"${localWorkspaceFolder}\" },\n")
	b.WriteString("  \"remoteEnv\": { \"WRAPTOOL_TOKEN\": \"${localEnv:WRAPTOOL_TOKEN}\" },\n")

	b.WriteString("\n  // Share the host harness-state pool so the harness's auth/skills/config persist\n")
	b.WriteString("  // across every project (the developer toolbox, mounted read-write).\n")
	b.WriteString("  \"mounts\": [\n")
	// Object form (not the "source=…,target=…" string) so a source path containing
	// a space — macOS ~/Library/Application Support — can't be mis-split by the
	// mount-string parser. target reuses devcontainerPoolHome, the single source of
	// the in-container HOME path.
	fmt.Fprintf(&b, "    { \"source\": %q, \"target\": %q, \"type\": \"bind\" }\n", poolHomeMountSource(), devcontainerPoolHome)
	b.WriteString("  ]\n")
	b.WriteString("}\n")
	return b.String()
}

// poolHomeMountSource returns the devcontainer bind-mount source for the shared
// harness pool home, derived from the host's real data dir so it is correct on
// every OS — matching where `wraptool up`'s ensureHarnessPool actually creates it
// (paths.DataDir(): Linux ~/.local/share, macOS ~/Library/Application Support,
// Windows %LOCALAPPDATA%). A hardcoded ~/.local/share works only on Linux; on
// macOS it points at a directory wraptool never creates, so the bind mount fails.
// The $HOME prefix is rewritten to ${localEnv:HOME} so the devcontainer CLI
// expands it client-side; if the data dir is not under $HOME we emit it literally.
func poolHomeMountSource() string {
	poolHome := filepath.Join(paths.DataDir(), "harness-pool", "home")
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, poolHome); err == nil && !strings.HasPrefix(rel, "..") {
			return "${localEnv:HOME}/" + filepath.ToSlash(rel)
		}
	}
	// ToSlash for forward slashes across OSes: the emitter uses %q so JSON escaping
	// is already handled, but a devcontainer/docker bind source is more portable
	// with forward slashes than Windows backslashes.
	return filepath.ToSlash(poolHome)
}
