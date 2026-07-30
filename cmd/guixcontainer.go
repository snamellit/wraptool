package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pti/wraptool/internal/harness"
	"github.com/pti/wraptool/internal/pool"
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

// bringUpGuixContainer runs the dev environment in a native Guix container
// (`guix shell --container`) built from the project's own manifest.scm — the
// same toolchain a developer uses, so the harness can compile/test/analyze
// locally. Credential isolation comes from the mount/user namespace (host $HOME
// and secrets are never shared), so privileged/remote git (push, private fetch)
// still goes through wraptool's MCP git tool, which holds the host credentials.
// --network (-N) gives outbound internet for dependency pulls, at the cost of
// sharing the host network namespace. See design/harness/guix-container-runtime.md.
func bringUpGuixContainer(root, token, listen, hname string, shell bool) error {
	manifest := filepath.Join(root, "manifest.scm")
	if !fileExists(manifest) {
		return fmt.Errorf("no manifest.scm at %s — the guix runtime builds the "+
			"container from it; add one, or use --runtime=devcontainer", root)
	}

	// Under -N the container shares the host loopback, so the harness reaches
	// wraptool at the server's own listen address (a wildcard bind maps to
	// loopback via probeAddr). The workspace is shared writable, so write the
	// MCP client config on the host — no in-container tooling needed.
	base := "http://" + probeAddr(listen)
	mcpPath, err := harness.Write(hname, harness.Params{
		Transport: "sse",
		URL:       base + "/sse",
		Token:     token,
		CWD:       root, // host worktree path -> ?cwd= scoping
		Dir:       root, // write the harness config into the shared workspace root
	})
	if err != nil {
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
		if err := pre.Run(); err != nil {
			return fmt.Errorf("realizing guix container profile: %w", err)
		}
		fmt.Printf("\nguix container prepared. Enter it with:\n  wraptool up --runtime=guix\n")
		return nil
	}

	// The developer harness pool: shared across all projects so a harness is
	// installed once, not per session. bin/ goes on PATH (mounted read-only —
	// installs happen host-side via `wraptool harness install`); home/ is the
	// harness HOME so its config/auth persist (writable). It is the developer's
	// toolbox — not the project (manifest.scm) and not host credentials.
	pool, poolBin, poolLib, poolHome, err := ensureHarnessPool()
	if err != nil {
		return err
	}
	fmt.Printf("harness pool: %s (install harness binaries into %s)\n", pool, poolBin)

	entry := "export WRAPTOOL_CONTAINER=1" +
		// Guix defaults TERM=dumb (no colors) and the box has no terminfo DB, so
		// normalize to a widely-assumed value; COLORTERM (forwarded below) drives
		// truecolor detection for the harness.
		"; export TERM=xterm-256color" +
		// Append (not prepend) the pool bin dir so a pool binary can never shadow a
		// toolchain binary (git/go/bash) — otherwise a poisoned pool could hijack
		// what every project's container runs.
		"; export PATH=$PATH:" + shellQuote(poolBin) +
		"; export HOME=" + shellQuote(poolHome) +
		"; cd " + shellQuote(root) + "; exec bash -l"
	args := []string{
		"shell", "--container", "--network",
		"--emulate-fhs",                // FHS layout (/usr/bin/env, /lib64/ld-linux-*)
		"--share=" + root + "=" + root, // workspace rw, host path preserved → ?cwd= matches
		// Harness pool, split by trust: bin/ and lib/ are read-only (--expose) so a compromised
		// agent in one project can't trojan the binaries/libraries other projects run; home/
		// (auth + state) stays writable (--share) so login/config persist per session.
		"--expose=" + poolBin + "=" + poolBin,
		"--expose=" + poolLib + "=" + poolLib,
		"--share=" + poolHome + "=" + poolHome,
		"--preserve=^WRAPTOOL_",  // forward WRAPTOOL_TOKEN / _CWD / _CONTAINER
		"--preserve=^COLORTERM$", // truecolor detection for the harness
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

// devcontainerPoolHome is the fixed in-container path the wraptool feature pins
// HOME to; `wraptool up` bind-mounts the host pool home/ onto it so harness
// auth/state persists across projects. Must match the feature's containerEnv.HOME
// in features/src/wraptool/devcontainer-feature.json.
//
// A dedicated user-space home (under /home, not /opt) — /opt connotes a shared
// *system* location, but this is the single harness user's HOME inside a
// throwaway, single-user container. The actual per-developer isolation boundary
// is the HOST source of the mount (pool.Root()'s underlying paths.DataDir() =
// XDG data, one per account), not this container-internal path.
const devcontainerPoolHome = "/home/wraptool-harness"

// harnessPoolDir is the developer-scoped pool of AI-harness binaries and
// their state, shared into every container — the same root the
// version-locked pool (internal/pool, cmd/harness.go) lives under; legacy
// bin/+lib/ sit alongside manifest.yaml/harness-lock.json/runtime/ at that
// one root (design/unified-harness-pool.md §16 migration). pool.Root() is
// the single source of truth for this path; this function no longer
// re-derives it.
func harnessPoolDir() string { return pool.Root() }

// ensureHarnessPool creates and returns the pool root plus its bin/ (PATH),
// lib/ (node_modules), and home/ (harness HOME) subdirectories.
func ensureHarnessPool() (pool, bin, lib, home string, err error) {
	pool = harnessPoolDir()
	bin = filepath.Join(pool, "bin")
	lib = filepath.Join(pool, "lib")
	home = filepath.Join(pool, "home")
	for _, d := range []string{bin, lib, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", "", "", "", fmt.Errorf("creating harness pool %s: %w", d, err)
		}
	}
	return pool, bin, lib, home, nil
}

// shellQuote wraps s as a single-quoted POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
