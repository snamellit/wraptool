package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// containerRuntime selects which backend `wraptool up` uses to bring up the
// isolated dev environment. Named to avoid shadowing the stdlib `runtime`.
type containerRuntime int

const (
	runtimeGuix containerRuntime = iota
	runtimeDevcontainer
)

func (r containerRuntime) String() string {
	switch r {
	case runtimeGuix:
		return "guix"
	case runtimeDevcontainer:
		return "devcontainer"
	default:
		return fmt.Sprintf("containerRuntime(%d)", int(r))
	}
}

// selectRuntime resolves the --runtime flag. Guix first: `auto` picks the Guix
// backend when the guix CLI is present AND the worktree has a manifest.scm to
// build the environment from; otherwise it falls back to devcontainer. An
// explicit "guix"/"devcontainer" overrides the probe.
func selectRuntime(mode, root string) (containerRuntime, error) {
	switch mode {
	case "guix":
		return runtimeGuix, nil
	case "devcontainer":
		return runtimeDevcontainer, nil
	case "", "auto":
		if hasGuix() && fileExists(filepath.Join(root, "manifest.scm")) {
			return runtimeGuix, nil
		}
		return runtimeDevcontainer, nil
	default:
		return 0, fmt.Errorf("unknown --runtime %q (want auto, guix, or devcontainer)", mode)
	}
}

// hasGuix reports whether the guix CLI is on PATH. A var so tests can stub the
// `auto` runtime probe without depending on the host having (or lacking) guix.
var hasGuix = func() bool { _, err := exec.LookPath("guix"); return err == nil }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// candidateManifests lists, in priority order, where the harness-runtime
// manifest for the Guix container may live. This is NOT the dev toolchain
// (manifest.scm): it is the box the AI harness runs in — node/bash/coreutils/
// nss-certs, deliberately no git/creds. See design/guix-container-runtime.md.
func candidateManifests(root string) []string {
	return []string{
		filepath.Join(root, ".wraptool", "container.scm"),
		filepath.Join(root, "container.scm"),
	}
}

func containerManifest(root string) string {
	for _, p := range candidateManifests(root) {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// containerManifestTemplate is the starter harness-runtime manifest written by
// `wraptool up --scaffold-guix`. It is the AI-harness box (node = Claude Code),
// deliberately WITHOUT git or credentials — privileged tools run host-side.
const containerManifestTemplate = `;; Runtime for the wraptool Guix dev container (` + "`wraptool up`" + `, guix runtime).
;;
;; This is the AI-harness box, NOT the dev toolchain (that is manifest.scm).
;; Deliberately has NO git and NO credentials: privileged tools (git, gcloud,
;; kubectl, ...) run on the host and are reached only through wraptool's MCP
;; tools. Keep it minimal; add only what the harness itself needs.
(specifications->manifest
 '("node"        ; Claude Code
   "bash"
   "coreutils"   ; the harness/shell prep expects mkdir/mv/...
   "nss-certs")) ; CA roots for HTTPS dependency pulls (npm/pip/cargo/...)
`

// scaffoldGuixContainer writes the starter .wraptool/container.scm if it (and a
// top-level container.scm) do not already exist, and returns the manifest path.
// Existing manifests are left untouched.
func scaffoldGuixContainer(root string) (string, error) {
	if existing := containerManifest(root); existing != "" {
		fmt.Printf("container manifest already exists: %s (leaving as-is)\n", existing)
		return existing, nil
	}
	dir := filepath.Join(root, ".wraptool")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, "container.scm")
	if err := os.WriteFile(path, []byte(containerManifestTemplate), 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Printf("scaffolded %s (harness runtime; edit to add tools)\n", path)
	return path, nil
}

// bringUpGuixContainer runs the dev environment in a native Guix container
// (`guix shell --container`). Credential isolation comes from the mount/user
// namespace (host $HOME and secrets are not shared); --network (-N) gives the
// container outbound internet for dependency pulls, at the cost of sharing the
// host network namespace. See design/guix-container-runtime.md §3/§5.
func bringUpGuixContainer(root, token, listen string, shell bool) error {
	manifest := containerManifest(root)
	if manifest == "" {
		return fmt.Errorf(
			"no container manifest found (looked for %s)\n\n"+
				"Create one — the AI-harness runtime, e.g.\n"+
				"    (specifications->manifest '(\"node\" \"bash\" \"coreutils\" \"nss-certs\"))\n"+
				"deliberately with no git/credentials — then re-run `wraptool up`.\n"+
				"(A scaffold command lands in a later phase; see design/guix-container-runtime.md §6.4.)",
			strings.Join(candidateManifests(root), ", "))
	}

	// Under -N the container shares the host loopback, so the harness reaches
	// wraptool at the server's own listen address (a wildcard bind maps to
	// loopback via probeAddr). The workspace is shared writable, so write the
	// MCP client config on the host — no in-container tooling needed.
	base := "http://" + probeAddr(listen)
	mcpPath := filepath.Join(root, ".mcp.json")
	if err := writeMCPConfig(mcpPath, base, token, root); err != nil {
		return err
	}
	fmt.Printf("wrote %s (wraptool @ %s)\n", mcpPath, base)

	fmt.Printf("bringing up guix container for %s (manifest %s) ...\n", root, manifest)
	fmt.Printf("note: --network shares the host network namespace (internet + host loopback " +
		"reachable); credentials stay isolated via the mount namespace.\n")

	if !shell {
		// A `guix shell --container` lives only for the duration of its command,
		// so there is no persistent container to leave running without a shell.
		// Warm the profile so the interactive entry is fast, then report.
		fmt.Println("realizing container profile ...")
		pre := exec.Command("guix", "shell", "-m", manifest, "--", "true")
		pre.Stdout, pre.Stderr = os.Stdout, os.Stderr
		_ = pre.Run() // best-effort cache warm; non-fatal
		fmt.Printf("\nguix container prepared. Enter it with:\n  wraptool up --runtime=guix\n")
		return nil
	}

	entry := "export WRAPTOOL_CONTAINER=1; cd " + shellQuote(root) + "; exec bash -l"
	args := []string{
		"shell", "--container", "--network",
		"--share=" + root + "=" + root, // workspace rw, host path preserved → ?cwd= matches
		"--preserve=^WRAPTOOL_",        // forward WRAPTOOL_TOKEN / _CWD / _CONTAINER
		"-m", manifest,
		"--", "bash", "-lc", entry,
	}
	env := append(os.Environ(),
		"WRAPTOOL_TOKEN="+token,
		"WRAPTOOL_CWD="+root, // host path → server scopes tools to this worktree via ?cwd=
		"WRAPTOOL_CONTAINER=1",
	)

	fmt.Printf("\nopening a shell in the guix container (exit tears it down; re-enter with `wraptool up`)...\n")
	cmd := exec.Command("guix", args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// writeMCPConfig writes the harness's .mcp.json pointing at the host-side
// wraptool over SSE, scoped to hostCWD via ?cwd=. It is the Go twin of the
// wraptool-connect feature's connect.sh (used by the devcontainer path); the
// two must stay structurally in lockstep (asserted in tests).
func writeMCPConfig(path, baseURL, token, hostCWD string) error {
	sse := strings.TrimRight(baseURL, "/") + "/sse?cwd=" + url.QueryEscape(hostCWD)
	server := map[string]any{"type": "sse", "url": sse}
	if token != "" {
		server["headers"] = map[string]string{"Authorization": "Bearer " + token}
	}
	doc := map[string]any{"mcpServers": map[string]any{"wraptool": server}}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding mcp config: %w", err)
	}
	b = append(b, '\n')

	// Back up an existing config once (mirrors connect.sh) before overwriting.
	if fileExists(path) {
		if bak := path + ".wraptool-bak"; !fileExists(bak) {
			if cur, rerr := os.ReadFile(path); rerr == nil {
				_ = os.WriteFile(bak, cur, 0o600)
			}
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("writing mcp config: %w", err)
	}
	return os.Rename(tmp, path)
}

// shellQuote wraps s as a single-quoted POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
