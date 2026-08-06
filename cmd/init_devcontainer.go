package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pti/wraptool/internal/harness"
	"github.com/pti/wraptool/internal/paths"
	"github.com/pti/wraptool/internal/pool"

	"github.com/spf13/cobra"
)

// wraptoolFeatureRef is the wiring-only wraptool Dev Container feature major
// (design/harness/unified-harness-pool.md §12.3): it pins HOME/PATH and
// writes the MCP client config, but installs no harness binary. The binary
// comes from the host harness pool's `runtime/` tree, mounted read-only by
// `wraptool up` (or declared here for a standalone `devcontainer up`) — run
// `wraptool harness install <name>` on the host first. Pinned to the major so
// patch/minor updates flow in. The old per-image-provisioning major (`:0`)
// remains published for legacy configs but is not what this scaffolder emits.
const wraptoolFeatureRef = "forge.snamellit.com/pti/wraptool/wraptool:1"

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

// The scaffolder no longer auto-adds the node feature for npm-installed
// harnesses (claude/opencode/pi): that existed only because the OLD
// provisioning feature ran `npm i -g` in-image at build time, and a missing
// npm failed the build. The wiring-only feature installs no harness binary at
// all — install.sh/connect.sh are POSIX sh with no npm/node dependency — and
// the harness binary itself now comes from the host harness-pool `runtime/`
// mount, which bundles its own pinned node (design/harness/unified-harness-pool.md
// §12.3, §16 Phase 4 item 2). Pass `--lang node` yourself if your PROJECT code
// needs a node toolchain; it is no longer implied by the harness choice.

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
language features you pick, the wiring-only wraptool feature with a chosen
harness, and the WRAPTOOL_* env / shared-pool mounts prefilled. Follows
docs/devcontainers.qmd (base image + features, not a fat all-in-one image).

It writes a NEW file and refuses to clobber an existing one (use --force). The
harness binary is NOT provisioned by this config: it comes from the host
harness pool's runtime/ tree — run 'wraptool harness install <harness>' on the
host, then 'wraptool up' (which mounts it) or plain 'devcontainer up' (which
needs the mount declared, as this scaffolder does).

Examples:
  # Go + Java project running Claude Code
  wraptool init devcontainer --harness claude --lang go,java

  # Node project running opencode
  wraptool init devcontainer --harness opencode --lang node`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInitDevcontainer(harnessName, langs, base, out, name, force)
		},
	}

	cmd.Flags().StringVar(&harnessName, "harness", "claude", "container harness to wire (claude, opencode, pi, agy, gemini)")
	cmd.Flags().StringSliceVar(&langs, "lang", nil, "language features to add (go, node, java, python, rust)")
	cmd.Flags().StringVar(&base, "base", "ubuntu", "base image: ubuntu, debian, alpine, or a full image ref")
	cmd.Flags().StringVar(&out, "out", ".devcontainer/devcontainer.json", "output path")
	cmd.Flags().StringVar(&name, "name", "", "dev container name (default: workspace directory name)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config")

	return cmd
}

func runInitDevcontainer(harnessName string, langs []string, base, out, name string, force bool) error {
	// Validate the harness: must be a container-CLI harness (the wraptool feature
	// wires an MCP config for a binary in the box; host IDEs like cursor/roo are
	// configured host-side with `wraptool init --harness`).
	d, ok := harness.Registry[harnessName]
	if !ok || !d.IsCLI() {
		return fmt.Errorf("harness %q is not a container-CLI harness; pick one of claude, opencode, pi, agy, gemini "+
			"(host IDEs are configured with `wraptool init --harness %s`)", harnessName, harnessName)
	}

	// Resolve + de-dup the language set. No auto-add here (see the doc comment
	// above languageFeatures): the harness binary no longer needs npm/node from
	// this image.
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
	// Emit languages in canonical order regardless of input order.
	sort.SliceStable(order, func(i, j int) bool { return langIndex(order[i]) < langIndex(order[j]) })

	if name == "" {
		name = deriveName(out)
	}

	content, err := renderDevcontainer(name, baseImage(base), order, harnessName, d.PoolLockable())
	if err != nil {
		return err
	}

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
		fmt.Printf("  languages: %s\n", strings.Join(order, ", "))
	}
	fmt.Printf("  harness: %s\n", harnessName)
	if d.PoolLockable() {
		fmt.Printf("\nOn the host: run `wraptool harness install %s` (once), export WRAPTOOL_TOKEN if\n", harnessName)
		fmt.Printf("your server uses auth, then open the folder in a Dev Container (or run `wraptool up`).\n")
	} else {
		fmt.Printf("\n%s is not pool-lockable — no `wraptool harness install` for it; bake its binary\n", harnessName)
		fmt.Printf("into your image yourself (or set \"connect\": false and wire it by hand). Export\n")
		fmt.Printf("WRAPTOOL_TOKEN if your server uses auth, then open the folder in a Dev Container\n")
		fmt.Printf("(or run `wraptool up`).\n")
	}
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
// for a human to edit, not machine state. Fails only when the runtime mount's
// source can't be safely rendered in string form (poolRuntimeMountString).
func renderDevcontainer(name, image string, langs []string, harnessName string, poolLockable bool) (string, error) {
	var b strings.Builder
	b.WriteString("{\n")
	b.WriteString("  // Generated by `wraptool init devcontainer` — a starting point; edit freely.\n")
	fmt.Fprintf(&b, "  %q: %q,\n", "name", name)
	fmt.Fprintf(&b, "  %q: %q,\n", "image", image)
	b.WriteString("\n  \"features\": {\n")
	for _, l := range langs {
		ref, _ := languageFeatureRef(l)
		fmt.Fprintf(&b, "    %q: {},\n", ref)
	}
	// The wraptool feature: wiring only (§12.3). It writes the MCP config and
	// pins HOME/PATH, but never installs the harness binary — that comes from
	// the runtime/ mount declared below.
	b.WriteString("    // wraptool: wiring only — MCP config + HOME/PATH pins. The harness\n")
	b.WriteString("    // binary comes from the host harness pool's runtime/ mount below, not\n")
	b.WriteString("    // this feature: run `wraptool harness install <name>` on the host.\n")
	if !poolLockable {
		// Not every harness has an automated pool installer (e.g. gemini) — the
		// runtime mount will never contain its binary, and there is no
		// `wraptool harness install` fallback to suggest. Correct guidance is
		// bake-it-in or wire-it-yourself, NOT "install it by hand inside the
		// container": with connect:true (this scaffold's default) the
		// feature's postCreate hard-fails before a hand-install could ever
		// happen (connect.sh's own PATH check).
		b.WriteString("    // this harness has no automated pool installer (not pool-lockable), so\n")
		b.WriteString("    // its binary is never in the runtime mount. Bake it into your image\n")
		b.WriteString("    // yourself, or set \"connect\": false below and wire the MCP config\n")
		b.WriteString("    // yourself once it's on PATH some other way.\n")
	}
	fmt.Fprintf(&b, "    %q: { \"harness\": %q }\n", wraptoolFeatureRef, harnessName)
	b.WriteString("  },\n")

	b.WriteString("\n  // WRAPTOOL_CWD scopes host tools to this worktree. WRAPTOOL_TOKEN authenticates\n")
	b.WriteString("  // to an auth-enabled server — kept out of the image via remoteEnv. Export it on the\n")
	fmt.Fprintf(&b, "  // host first: export WRAPTOOL_TOKEN=$(cat %s/mcp-token)\n", paths.ConfigDir())
	b.WriteString("  \"containerEnv\": { \"WRAPTOOL_CWD\": \"${localWorkspaceFolder}\" },\n")
	b.WriteString("  \"remoteEnv\": { \"WRAPTOOL_TOKEN\": \"${localEnv:WRAPTOOL_TOKEN}\" },\n")

	homeSrc, homeRelative := poolMountSourceOf(pool.HomeDir())
	runtimeSrc, runtimeRelative := poolMountSourceOf(pool.RuntimeDir())
	runtimeMount, err := poolRuntimeMountString(runtimeSrc)
	if err != nil {
		return "", err
	}

	b.WriteString("\n  // Share the host harness pool (design/harness/unified-harness-pool.md §12.3):\n")
	b.WriteString("  // home/ (auth/skills/config) is read-write so it persists across every\n")
	b.WriteString("  // project; runtime/ (the version-locked harness binaries) is read-only —\n")
	b.WriteString("  // the pool is a cross-project trust domain the container must never write.\n")
	b.WriteString("  // The runtime entry MUST stay the string form below: it is CSV-parsed by\n")
	b.WriteString("  // the devcontainer CLI (so an interior space, e.g. macOS's own\n")
	b.WriteString("  // ~/Library/Application Support, survives intact) and it is the ONLY form\n")
	b.WriteString("  // the CLI honors a read-only flag on — object-form mounts have no\n")
	b.WriteString("  // read-only field at all in its schema and silently come up read-write\n")
	b.WriteString("  // regardless of any \"readonly\" key. home/ is read-write either way, so it\n")
	b.WriteString("  // stays object form purely for readability.\n")
	if !homeRelative || !runtimeRelative {
		b.WriteString("  // NOTE: this host's harness-pool data dir is not under $HOME, so the\n")
		b.WriteString("  // source below is a literal absolute path — correct only on THIS machine.\n")
		b.WriteString("  // A teammate on a different host/OS must adjust it (§12.3 mixed-OS limits).\n")
	}
	b.WriteString("  \"mounts\": [\n")
	fmt.Fprintf(&b, "    { \"source\": %q, \"target\": %q, \"type\": \"bind\" },\n", homeSrc, devcontainerPoolHome)
	fmt.Fprintf(&b, "    %q\n", runtimeMount)
	b.WriteString("  ]\n")
	b.WriteString("}\n")
	return b.String(), nil
}

// poolMountSourceOf returns the devcontainer bind-mount source for a
// harness-pool host path, and whether that source is `${localEnv:HOME}`-relative
// (true) or a literal, ToSlash'd absolute path (false) — derived from the
// host's real pool paths so it is correct on every OS (paths.DataDir(): Linux
// ~/.local/share, macOS ~/Library/Application Support, Windows
// %LOCALAPPDATA%). A hardcoded ~/.local/share works only on Linux; on macOS it
// points at a directory wraptool never creates, so the bind mount fails. The
// $HOME prefix is rewritten to ${localEnv:HOME} so the devcontainer CLI
// expands it client-side (§12.3: this holds for the Linux/macOS defaults, and
// for Windows only when HOME is set); when the data dir is not under $HOME we
// emit the literal host path instead, and the caller documents the caveat.
func poolMountSourceOf(hostPath string) (source string, homeRelative bool) {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, hostPath); err == nil && !strings.HasPrefix(rel, "..") {
			return "${localEnv:HOME}/" + filepath.ToSlash(rel), true
		}
	}
	// ToSlash for forward slashes across OSes: the emitter uses %q so JSON escaping
	// is already handled, but a devcontainer/docker bind source is more portable
	// with forward slashes than Windows backslashes.
	return filepath.ToSlash(hostPath), false
}

// poolHomeMountSource returns the devcontainer bind-mount source for the
// shared harness-pool home/ — kept as its own named function since tests here
// and cmd/up.go's devcontainerLocalEnvRE doc comment reference it by name.
func poolHomeMountSource() string {
	src, _ := poolMountSourceOf(pool.HomeDir())
	return src
}

// poolRuntimeMountSourceUnsafeChars are the characters that would corrupt the
// runtime mount's CSV "source=...,target=...,type=bind,readonly" string form
// (§12.3's CRITICAL FORM RULING — the only form the devcontainer CLI honors a
// read-only flag on, see renderDevcontainer's own comment): a comma is the
// CSV field separator the devcontainer CLI's mount-string parser splits on —
// a comma in the path would silently misparse into the wrong target/type —
// and a quote or backslash needs escaping that CSV mount strings don't
// support at all. Mirrors cmd/up.go's devcontainerRuntimeMountSnippet guard
// (which protects a printed, human-pasted snippet); here the corrupted output
// would instead be written straight into a machine-generated devcontainer.json.
const poolRuntimeMountSourceUnsafeChars = `,"\`

// poolRuntimeMountString builds the runtime mount's string-form entry, or
// refuses with an actionable error when src contains a character that would
// corrupt it (poolRuntimeMountSourceUnsafeChars) — silently emitting a broken
// mounts entry would be worse than failing the scaffold outright. A wraptool
// data dir landing on such a path is unusual (an XDG_DATA_HOME override, or a
// deliberately awkward directory name) but not impossible; relocating
// XDG_DATA_HOME (or the platform-equivalent override) to a path without one
// of these characters is the fix.
func poolRuntimeMountString(src string) (string, error) {
	if strings.ContainsAny(src, poolRuntimeMountSourceUnsafeChars) {
		return "", fmt.Errorf("harness-pool runtime path %q contains a comma, quote, or backslash, which would "+
			"corrupt the devcontainer runtime mount's required string form (§12.3); relocate the pool by setting "+
			"XDG_DATA_HOME (or the platform-equivalent data-dir override) to a path without one", src)
	}
	return "source=" + src + ",target=" + devcontainerPoolRuntime + ",type=bind,readonly", nil
}
