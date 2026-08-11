package doctor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

const envContainerBackend CheckID = "env.container-backend"

func init() {
	register(Meta{
		ID:      envContainerBackend,
		Needs:   NeedsNothing, // no inputs at all — this is why doctor works before any config exists
		Summary: "reports which container backends are on PATH",
	}, runEnvContainerBackend)
}

// envBackends is a fixed slice, never a map: map range order is randomized per
// iteration and TestChecksAreDeterministic runs every check twice and diffs.
// Order matches cmd/guixcontainer.go's and cmd/up.go's preference.
var envBackends = []string{"guix", "docker", "podman"}

// runEnvContainerBackend is a NAMED package-level function, not a closure inside
// init(): its test calls it directly with a hand-built *Input and never touches
// the registry, which is what stops one author's tests breaking when another
// merges.
func runEnvContainerBackend(_ context.Context, in *Input) Result {
	var present, absent []string
	for _, b := range envBackends {
		if p, err := in.Env.LookPath(b); err == nil {
			present = append(present, b+" ("+p+")")
		} else {
			absent = append(absent, b)
		}
	}
	f := Finding{
		// ID is stamped by the engine from Meta.ID.
		Severity: SeverityInfo,
		Title:    "Container backends",
		Detail:   envDescribeBackends(present, absent),
		DocRef:   "docs/isolated-environments.md",
	}
	if len(present) == 0 {
		// info, never warn (§7.1): a host-only deployment serving MCP to a
		// separately managed container is fully supported and must not be
		// nagged. env.no-backend-with-project (B1) is the check that judges,
		// and only once --workspace shows the operator opted into `up`.
		f.Remedy = "install guix, docker, or podman if you want `wraptool up` to build an isolated environment"
	}
	return Found(f)
}

func envDescribeBackends(present, absent []string) string {
	var b strings.Builder
	if len(present) > 0 {
		b.WriteString("found: ")
		b.WriteString(strings.Join(present, ", "))
	} else {
		b.WriteString("none found on PATH")
	}
	if len(absent) > 0 {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString("not found: ")
		b.WriteString(strings.Join(absent, ", "))
	}
	return b.String()
}

// ------------------------------------------------------- env.devcontainer-cli

const envDevcontainerCLI CheckID = "env.devcontainer-cli"

func init() {
	register(Meta{
		ID:      envDevcontainerCLI,
		Needs:   NeedsNothing, // same rationale as env.container-backend
		Summary: "reports whether the devcontainer CLI is on PATH",
	}, runEnvDevcontainerCLI)
}

// runEnvDevcontainerCLI always reports (§7.1): the devcontainer CLI is one of
// the two runtimes cmd/guixcontainer.go can select, so its presence or
// absence is worth knowing unconditionally, same as env.container-backend.
func runEnvDevcontainerCLI(_ context.Context, in *Input) Result {
	f := Finding{
		Severity: SeverityInfo,
		Title:    "devcontainer CLI",
		DocRef:   "docs/isolated-environments.md",
	}
	if p, err := in.Env.LookPath("devcontainer"); err == nil {
		f.Detail = fmt.Sprintf("found: devcontainer (%s)", p)
	} else {
		f.Detail = "devcontainer CLI not found on PATH"
		f.Remedy = "install the devcontainer CLI if you want `wraptool up --runtime=devcontainer` to build the isolated environment"
	}
	return Found(f)
}

// ---------------------------------------------------- env.no-backend-with-project

const envNoBackendWithProject CheckID = "env.no-backend-with-project"

func init() {
	register(Meta{
		ID:      envNoBackendWithProject,
		Needs:   NeedsWorkspace,
		Summary: "warns when a project workspace expects a container backend but none is on PATH",
	}, runEnvNoBackendWithProject)
}

// envProjectMarkers is the fixed, ordered list of files/dirs at the workspace
// root that mark a project as having opted into `wraptool up` (§7.1). Fixed
// order, never a map: TestChecksAreDeterministic runs every check 8 times.
var envProjectMarkers = []string{".devcontainer", "manifest.scm"}

// runEnvNoBackendWithProject fires only when the operator has demonstrably
// opted into `up` (a project marker is present) AND no backend at all is on
// PATH — a host-only deployment with no such markers must stay silent, same
// as env.container-backend's info-not-warn rationale.
func runEnvNoBackendWithProject(_ context.Context, in *Input) Result {
	found := envFoundMarkers(in.Env, in.Workspace)
	if len(found) == 0 {
		return Pass()
	}
	for _, b := range envBackends {
		if _, err := in.Env.LookPath(b); err == nil {
			return Pass() // a backend is present; nothing to warn about
		}
	}
	return Found(Finding{
		Severity: SeverityWarn,
		Title:    "No container backend for this project",
		Detail: fmt.Sprintf("%s has %s, but none of guix, docker, or podman is on PATH",
			in.Workspace, strings.Join(found, ", ")),
		Remedy: "install guix, docker, or podman so `wraptool up` can build the isolated environment this project expects",
		DocRef: "docs/isolated-environments.md",
	})
}

// envFoundMarkers reports which of envProjectMarkers exist directly under
// workspace, in envProjectMarkers' own order.
func envFoundMarkers(env Env, workspace string) []string {
	var found []string
	for _, m := range envProjectMarkers {
		if _, err := env.Stat(filepath.Join(workspace, m)); err == nil {
			found = append(found, m)
		}
	}
	return found
}

// ---------------------------------------------------- env.guix-runtime-selected

const envGuixRuntimeSelected CheckID = "env.guix-runtime-selected"

func init() {
	register(Meta{
		ID:      envGuixRuntimeSelected,
		Needs:   NeedsWorkspace,
		Summary: "reports when the guix runtime will auto-select for this workspace",
	}, runEnvGuixRuntimeSelected)
}

// runEnvGuixRuntimeSelected mirrors cmd/guixcontainer.go's hasGuix/manifest.scm
// probe (`selectRuntime`'s "auto" branch): guix on PATH AND a manifest.scm at
// the workspace root. Silent otherwise — this is informational, not a
// judgement about the absence of either.
//
// The Detail is deliberately qualified to "under --runtime=auto (the
// default)": selectRuntime only runs this probe for mode "" or "auto" — an
// explicit `--runtime=devcontainer` bypasses it entirely, so a flat "wraptool
// up will use the guix runtime" would be wrong for that invocation.
func runEnvGuixRuntimeSelected(_ context.Context, in *Input) Result {
	if _, err := in.Env.LookPath("guix"); err != nil {
		return Pass()
	}
	if _, err := in.Env.Stat(filepath.Join(in.Workspace, "manifest.scm")); err != nil {
		return Pass()
	}
	return Found(Finding{
		Severity: SeverityInfo,
		Title:    "guix runtime will auto-select",
		Detail: fmt.Sprintf("guix is on PATH and %s has manifest.scm — under --runtime=auto (the default), "+
			"`wraptool up` will use the guix runtime", in.Workspace),
		DocRef: "docs/isolated-environments.md",
	})
}

// ------------------------------------------------------------ env.xdg-writable

const envXDGWritable CheckID = "env.xdg-writable"

func init() {
	register(Meta{
		ID:      envXDGWritable,
		Needs:   NeedsModeBits, // a writability verdict needs POSIX permission bits (§12, Windows)
		Summary: "checks that the nearest existing ancestor of each XDG directory is writable",
	}, runEnvXDGWritable)
}

// envXDGDir names one of §7.1's three directories plus the label its finding
// text uses. Fixed slice, never a map, for the same determinism reason as
// envProjectMarkers.
type envXDGDir struct {
	label string
	path  string
}

// runEnvXDGWritable checks each of §7.1's three directories. Existence isn't
// required (these are created lazily, internal/config/scaffold.go:58, and
// RuntimeDir() falls back to CacheDir()/run when XDG_RUNTIME_DIR is unset,
// internal/paths/paths.go:48-53), so the check walks up to the nearest
// ancestor that DOES exist and asks whether that one is writable.
func runEnvXDGWritable(_ context.Context, in *Input) Result {
	dirs := []envXDGDir{
		{"config", in.Paths.ConfigDir},
		{"data", in.Paths.DataDir},
		{"runtime", in.Paths.RuntimeDir},
	}
	var findings []Finding
	for _, d := range dirs {
		if d.path == "" {
			continue
		}
		fi, ok := envNearestExistingAncestor(in.Env, d.path)
		if !ok {
			// No ancestor at all exists (not even the root) — nothing to check;
			// a real filesystem always has a root, this is only reachable
			// against a hand-built fixture missing one.
			continue
		}
		if envDirWritable(fi, in.Env.Getuid(), in.Env.Getgid()) {
			continue
		}
		findings = append(findings, Finding{
			Severity: SeverityFail,
			Title:    "XDG directory not writable",
			// fi.Mode, not fi.Perm(): Perm() clears the type bits, so a
			// directory would render as "-r-x------" — a leading dash reading
			// as a regular file in the operator-facing report.
			Detail: fmt.Sprintf("%s dir %s: nearest existing ancestor %s (mode %s) is not writable",
				d.label, d.path, fi.Path, fi.Mode),
			Remedy: fmt.Sprintf("make %s writable by the current user, or point the relevant XDG_* variable elsewhere", fi.Path),
		})
	}
	return Found(findings...)
}

// envNearestExistingAncestor walks path upward, Stat-ing each ancestor until
// one exists (or the walk reaches a fixed point, i.e. the root). ok is false
// only when not even the root exists — see runEnvXDGWritable.
func envNearestExistingAncestor(env Env, path string) (FileInfo, bool) {
	cur := path
	for {
		if fi, err := env.Stat(cur); err == nil {
			return fi, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return FileInfo{}, false
		}
		cur = parent
	}
}

// envDirWritable decides writability from the classic owner/group/other rule:
// the owner bit if uid matches, else the group bit if gid matches, else the
// other bit. When ownership is unknown (FileInfo.OwnerKnown() false — no
// syscall.Stat_t, e.g. a platform host_stat_other.go covers) it falls back to
// "any write bit at all", which is the best a mode-bits-only signal can do.
//
// uid 0 (root) is an explicit case, checked before any mode bit: POSIX write
// permission checks are bypassed entirely for root, so `sudo wraptool doctor`
// auditing a 0700 directory owned by uid 1000 must never FAIL it as
// unwritable — the process can write there regardless of what the mode says.
//
// gid membership is necessarily approximate: Env has no supplementary-group
// accessor (host.go, CLOSED — plan §1 rule 10), only the primary gid, so a
// mismatch there does not prove the operator lacks group access. A set
// group-write bit is therefore treated as writable even when gid does not
// match the file's own primary GID (the "default" branch below) — the
// operator may still reach it via a supplementary group this package cannot
// see, and a false FAIL is worse than an occasional false PASS here.
func envDirWritable(fi FileInfo, uid, gid int) bool {
	if uid == 0 {
		return true
	}
	perm := fi.Perm()
	switch {
	case !fi.OwnerKnown():
		return perm&0o222 != 0
	case uid >= 0 && fi.UID == uid:
		return perm&0o200 != 0
	case gid >= 0 && fi.GID == gid:
		return perm&0o020 != 0
	default:
		if perm&0o020 != 0 {
			return true // group-write bit set; a supplementary group might match
		}
		return perm&0o002 != 0
	}
}
