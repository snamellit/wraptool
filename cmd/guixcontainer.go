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
	// installed once, not per session. home/ is always the harness HOME so its
	// config/auth persist (writable). Which pool subtree provides the harness
	// binaries — the locked runtime/ tree or the legacy bin/+lib/ tree — is
	// decided by resolveGuixPoolPlan below (design/harness/unified-harness-pool.md
	// §12.1). It is the developer's toolbox — not the project (manifest.scm)
	// and not host credentials.
	poolRoot, poolBin, poolLib, poolHome, err := ensureHarnessPool()
	if err != nil {
		return err
	}

	// Active-pointer lookup: HostPlatformKey errors only for an unsupported
	// host arch, which resolveGuixPoolPlan treats the same as "no pointer"
	// (empty pointerPath fails the read) — §12.1 point 3, never fail `up` for
	// this. hname is already validated against harness.Registry by the
	// harness.Write call above, so the map lookup below never misses.
	var pointerPath string
	if key, err := pool.HostPlatformKey(); err == nil {
		pointerPath = pool.PointerFile(key, hname)
	}
	// hname was validated by the harness.Write call above, so the lookup
	// cannot miss today — but a missing key would silently yield a zero
	// Descriptor (non-lockable, quiet legacy plan) rather than an error,
	// so make the dependency on that validation explicit.
	desc, knownHarness := harness.Registry[hname]
	if !knownHarness {
		return fmt.Errorf("unknown harness %q resolving the pool plan (registry lookup after validation — this is a bug)", hname)
	}
	plan := resolveGuixPoolPlan(guixPoolPlanInput{
		poolPlanInput: poolPlanInput{
			Harness:               hname,
			PoolLockable:          desc.PoolLockable(),
			PointerPath:           pointerPath,
			RuntimeDir:            pool.RuntimeDir(),
			RuntimeBinDir:         pool.RuntimeBinDir(),
			RuntimeGenerationsDir: pool.RuntimeGenerationsDir(),
		},
		LegacyBin: poolBin,
		LegacyLib: poolLib,
	})
	if plan.notice != "" {
		fmt.Fprintln(os.Stderr, plan.notice)
	}
	// The legacy bin/ drop-in dir mention stays accurate in BOTH modes
	// (wave2-integration-fixes item 3): legacy bin/+lib/ is genuinely
	// mounted whether or not an active pointer also mounted runtime/, so
	// telling the developer where to drop a hand-managed binary is correct
	// either way — runtime/bin (the generated dispatcher dir, when active)
	// is never offered as that location, only poolBin.
	if plan.active {
		fmt.Printf("harness pool: %s (legacy bin/ also mounted for other harnesses/hand-dropped tools: %s)\n", poolRoot, poolBin)
	} else {
		fmt.Printf("harness pool: %s (install harness binaries into %s)\n", poolRoot, poolBin)
	}

	entry := buildContainerEntry(root, poolHome, plan.pathDirs)
	args := buildGuixShellArgs(root, manifest, poolHome, plan, entry)
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

// devcontainerPoolRuntime is the fixed in-container path the locked
// harness-pool runtime/ tree (§5, §6.3) is bind-mounted read-only onto, when
// `wraptool up` finds an active pointer for the selected harness
// (design/harness/unified-harness-pool.md §12.2). Under /opt (not /home,
// unlike devcontainerPoolHome above) because this is genuinely a
// system-toolbox mount — read-only, shared by every harness, unrelated to any
// user's HOME — mirroring the Guix runtime's own runtime/ mount semantics
// (§12.1). devcontainerPoolRuntimeBin is the generated-dispatcher directory
// appended to PATH.
const (
	devcontainerPoolRuntime    = "/opt/wraptool-harness/runtime"
	devcontainerPoolRuntimeBin = devcontainerPoolRuntime + "/bin"
)

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

// guixPoolPlan is the harness-pool mount/PATH/notice decision for one guix
// container invocation (design/harness/unified-harness-pool.md §12.1,
// WIDENED by wave2-integration-fixes item 3): when an active pointer exists
// for the host platform key and its generation is present on disk, the
// locked runtime/ tree, read-only, is mounted ALONGSIDE (not instead of) the
// legacy bin/+lib/ tree — mixed-state Phase 2 RULING: activating one
// harness must not silently drop every OTHER harness's binary (or a
// hand-dropped tool) from the container. Whenever no active pointer exists
// at all, only the legacy bin/+lib/ tree is mounted, with a stderr
// deprecation notice (§16 migration). home/ is not part of the plan — it
// stays shared read-write unchanged in either mode (handled directly in
// bringUpGuixContainer/buildGuixShellArgs). Full Phase-4 cutover drops the
// legacy exposes/PATH entry entirely (§16 Phase 4 item 5: "gc learns to
// remove legacy bin/+lib/"); until then this stays a superset, never a
// replacement.
type guixPoolPlan struct {
	exposes []string // "--expose=host=host" args, read-only, same-path (§6.3)
	// pathDirs are appended (never prepended) to PATH, in order. Active mode
	// is [runtimeBinDir, legacyBin] — the freshly activated generation wins
	// PATH resolution for THIS harness (runtime/bin listed first) while
	// every other harness's legacy binary stays reachable right after it;
	// legacy-only mode is [legacyBin] alone.
	pathDirs []string
	active   bool   // true: runtime/ tree ALSO mounted, alongside legacy bin/+lib/ (mixed-state Phase 2); false: legacy bin/+lib/ only
	notice   string // non-empty: legacy-fallback deprecation notice for stderr (legacy-only mode)
}

// poolPlanInput is the harness-pool-plan input SHARED by the guix and
// devcontainer runtimes' own pool-plan input structs (each embeds it): the
// harness identity/lockability plus the active-pointer/runtime-tree paths
// both resolveGuixPoolPlan (below) and resolveDevcontainerPoolPlan
// (cmd/up.go) need to decide "is there a live locked generation for this
// harness on this platform" — the two runtimes' plans differ only in what
// they DO with that answer (guix mounts runtime/ itself; devcontainer
// verifies a config-declared mount, §12.2's Phase-4 ruling), never in how
// liveness itself is determined, so this is one type, not two independently
// maintained ones. Named fields replace positional same-typed strings — the
// untested call site is exactly where several interchangeable path strings
// transpose silently (pattern: internal/pool.PoolPaths). All fields are
// plain strings/bool so tests build one against a t.TempDir() layout, with
// no real $XDG_DATA_HOME and no container-runtime exec.
type poolPlanInput struct {
	// Harness is the harness name — the dispatcher/generation entrypoint file
	// name, and the name cited in any deprecation notice.
	Harness string
	// PoolLockable is true when Harness is pool-lockable
	// (harness.Registry[Harness].PoolLockable(): an npm package or an
	// archive source). Non-lockable harnesses (gemini/cursor/roo today)
	// have no runtime/ path to move to, so their legacy fallback is silent —
	// a notice promising a fix command that can't exist for them would be
	// noise (§16).
	PoolLockable bool
	// PointerPath is pool.PointerFile(<target platform key>, Harness), or ""
	// when the platform key itself could not be resolved — treated
	// identically to "no active pointer" (§12.1 point 3).
	PointerPath string
	// RuntimeDir is pool.RuntimeDir() — mounted read-only at the same host
	// path when the guix plan is active, or printed as the concrete mount
	// source in the devcontainer verify-notice.
	RuntimeDir string
	// RuntimeBinDir is pool.RuntimeBinDir(), the dispatcher directory:
	// RuntimeBinDir/Harness (the generated dispatcher script) must exist for
	// the pointer to be considered live.
	RuntimeBinDir string
	// RuntimeGenerationsDir is pool.RuntimeGenerationsDir(); the pointer's
	// referenced generation's own RuntimeGenerationsDir/<genID>/bin/Harness
	// must also exist (§5, §11.4) — a pointer can outlive its generation
	// (e.g. deleted by a concurrent `gc`).
	RuntimeGenerationsDir string
}

// guixPoolPlanInput bundles poolPlanInput with the guix-only legacy bin/+lib/
// paths resolveGuixPoolPlan also needs.
type guixPoolPlanInput struct {
	poolPlanInput
	// LegacyBin, LegacyLib are the pre-existing pool bin/+lib/ dirs.
	LegacyBin string
	LegacyLib string
}

// resolveGuixPoolPlan decides guixPoolPlan for in.Harness. Pure aside from
// the pool.ReadPointer file read and the two existence checks below, so
// tests drive it against a t.TempDir() pool layout with no guix exec.
func resolveGuixPoolPlan(in guixPoolPlanInput) guixPoolPlan {
	if in.PointerPath != "" {
		if raw, err := pool.ReadPointer(in.PointerPath); err == nil {
			// A missing file, an unreadable path (e.g. a directory), or an
			// empty/whitespace-only generation id all count as "no active
			// pointer" (§12.1 point 2). TrimSpace (not just ReadPointer's own
			// single-trailing-newline trim) so "\n\n" or " " cannot activate
			// the runtime plan.
			if genID := strings.TrimSpace(raw); genID != "" {
				dispatcher := filepath.Join(in.RuntimeBinDir, in.Harness)
				genBin := filepath.Join(in.RuntimeGenerationsDir, genID, "bin", in.Harness)
				if fileExists(dispatcher) && fileExists(genBin) {
					return guixPoolPlan{
						// Mixed-state Phase 2 (wave2-integration-fixes item 3,
						// §12.1 RULING): runtime/ AND legacy bin/+lib/ are
						// BOTH mounted — activating this harness must not
						// drop every other harness's legacy binary (or a
						// hand-dropped tool) from the container. See
						// guixPoolPlan's own doc comment.
						exposes: []string{
							"--expose=" + in.RuntimeDir + "=" + in.RuntimeDir,
							"--expose=" + in.LegacyBin + "=" + in.LegacyBin,
							"--expose=" + in.LegacyLib + "=" + in.LegacyLib,
						},
						pathDirs: []string{in.RuntimeBinDir, in.LegacyBin},
						active:   true,
					}
				}
				// The pointer resolved but its generation is gone from disk
				// (e.g. a concurrent gc) — fall back, but with a distinct
				// notice naming what actually went wrong.
				return legacyGuixPoolPlan(in, fmt.Sprintf(
					"notice: active generation %s for %s is missing on disk; using legacy pool — "+
						"run `wraptool harness sync` to rebuild", genID, in.Harness))
			}
		}
	}
	return legacyGuixPoolPlan(in, fmt.Sprintf(
		"notice: using legacy harness pool for %s; run `wraptool harness install %s` (or `wraptool harness "+
			"sync` if already installed) to build a locked generation (legacy path is removed in a future release)",
		in.Harness, in.Harness))
}

// legacyGuixPoolPlan builds the legacy bin/+lib/ plan, gating notice on
// in.PoolLockable — a harness with no pool package at all has no runtime/
// path to move to, so it takes the legacy path silently (§16).
func legacyGuixPoolPlan(in guixPoolPlanInput, notice string) guixPoolPlan {
	plan := guixPoolPlan{
		exposes: []string{
			"--expose=" + in.LegacyBin + "=" + in.LegacyBin,
			"--expose=" + in.LegacyLib + "=" + in.LegacyLib,
		},
		pathDirs: []string{in.LegacyBin},
	}
	if in.PoolLockable {
		plan.notice = notice
	}
	return plan
}

// buildContainerEntry builds the interactive shell entrypoint run inside the
// guix container: normalizes TERM (guix defaults to TERM=dumb with no
// terminfo DB in the container), appends (never prepends) every pathDirs
// entry to PATH, in order, so a pool tool can never shadow a project
// toolchain binary (git/go/bash) — otherwise a poisoned pool could hijack
// what every project's container runs — pins HOME to the shared pool home,
// and drops into the workspace. Pure string building, no exec, so tests can
// pin it exactly.
func buildContainerEntry(root, poolHome string, pathDirs []string) string {
	quoted := make([]string, 0, len(pathDirs))
	for _, dir := range pathDirs {
		quoted = append(quoted, shellQuote(dir))
	}
	pathAppend := ""
	if len(quoted) > 0 {
		pathAppend = ":" + strings.Join(quoted, ":")
	}
	return "export WRAPTOOL_CONTAINER=1" +
		"; export TERM=xterm-256color" +
		"; export PATH=$PATH" + pathAppend +
		"; export HOME=" + shellQuote(poolHome) +
		"; cd " + shellQuote(root) + "; exec bash -l"
}

// buildGuixShellArgs assembles the `guix shell --container` argv: workspace
// share, plan's exposes (runtime/ read-only when an active pointer was found,
// else the legacy bin/+lib/ read-only exposes), pool home/ read-write
// (unchanged in both modes, §12.1), env preservation, manifest, and the
// interactive entrypoint. Pure — no exec — so tests can pin it exactly.
func buildGuixShellArgs(root, manifest, poolHome string, plan guixPoolPlan, entry string) []string {
	args := []string{
		"shell", "--container", "--network",
		"--emulate-fhs",                // FHS layout (/usr/bin/env, /lib64/ld-linux-*)
		"--share=" + root + "=" + root, // workspace rw, host path preserved → ?cwd= matches
	}
	args = append(args, plan.exposes...)
	args = append(args,
		"--share="+poolHome+"="+poolHome, // auth + state stays writable so login/config persist per session
		"--preserve=^WRAPTOOL_",          // forward WRAPTOOL_TOKEN / _CWD / _CONTAINER
		"--preserve=^COLORTERM$",         // truecolor detection for the harness
		"-m", manifest,
		"--", "bash", "-lc", entry,
	)
	return args
}
