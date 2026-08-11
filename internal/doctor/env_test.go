package doctor

import (
	"context"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// TestRunEnvContainerBackendAllSubsets exercises all 2^3 present/absent
// combinations of guix/docker/podman with no real filesystem and no real
// PATH — every combination is a "finding" outcome (never "pass": the check
// always reports, §7.1), info severity, and a Remedy iff nothing is present.
func TestRunEnvContainerBackendAllSubsets(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		mask := mask
		t.Run("", func(t *testing.T) {
			env := NewMapEnv()
			want := map[string]bool{}
			for i, b := range envBackends {
				if mask&(1<<i) != 0 {
					env.PathBinaries[b] = "/opt/bin/" + b
					want[b] = true
				}
			}

			res := runEnvContainerBackend(context.Background(), &Input{Env: env})

			if res.Outcome() != OutcomeFinding {
				t.Fatalf("mask %03b: outcome %v, want finding", mask, res.Outcome())
			}
			if len(res.Findings) != 1 {
				t.Fatalf("mask %03b: got %d findings, want 1", mask, len(res.Findings))
			}
			f := res.Findings[0]
			if f.Severity != SeverityInfo {
				t.Errorf("mask %03b: severity %v, want info", mask, f.Severity)
			}
			anyPresent := len(want) > 0
			if (f.Remedy != "") == anyPresent {
				t.Errorf("mask %03b: remedy set=%v, anyPresent=%v", mask, f.Remedy != "", anyPresent)
			}

			// Assert on the rendered SEGMENTS, not mere substring presence: this
			// must fail if a backend that IS present ever renders in the "not
			// found" segment (or vice versa) — swapped present/absent arguments
			// are otherwise invisible to a plain substring check. found/notFound
			// below are the exact "found: "/"not found: " segments of Detail.
			found, notFound := envSplitBackendDetail(t, f.Detail)
			for _, b := range envBackends {
				if want[b] != slices.Contains(found, b) {
					t.Errorf("mask %03b: detail %q — found segment %v, want %q present=%v", mask, f.Detail, found, b, want[b])
				}
				if want[b] == slices.Contains(notFound, b) {
					t.Errorf("mask %03b: detail %q — not-found segment %v, want %q absent=%v", mask, f.Detail, notFound, b, !want[b])
				}
			}
		})
	}
}

// envSplitBackendDetail parses envDescribeBackends' rendered Detail back into
// its "found: a, b" and "not found: c" backend lists, so the test can assert
// each backend landed on the correct side rather than merely appearing
// somewhere in the string.
func envSplitBackendDetail(t *testing.T, detail string) (found, notFound []string) {
	t.Helper()
	for _, seg := range strings.Split(detail, "; ") {
		switch {
		case strings.HasPrefix(seg, "found: "):
			found = envParseBackendNames(strings.TrimPrefix(seg, "found: "))
		case strings.HasPrefix(seg, "not found: "):
			notFound = envParseBackendNames(strings.TrimPrefix(seg, "not found: "))
		case seg == "none found on PATH":
			// no present backends; found stays nil
		default:
			t.Fatalf("unrecognized Detail segment %q in %q", seg, detail)
		}
	}
	return found, notFound
}

// envParseBackendNames strips each entry's " (path)" suffix (present backends
// are rendered as "guix (/opt/bin/guix)") to recover the bare name.
func envParseBackendNames(list string) []string {
	parts := strings.Split(list, ", ")
	out := make([]string, len(parts))
	for i, p := range parts {
		name, _, _ := strings.Cut(p, " (")
		out[i] = name
	}
	return out
}

// env.container-backend's registration presence and Meta shape (a non-empty
// Summary, a known group, needing nothing) are already covered registry-WIDE
// by TestRegistryIsWellFormed (invariants_test.go) and
// TestChecksSurviveAVirginHost's virgin-host case (which only reaches this
// check at all because it needs nothing) — asserting them again here, over
// the global Registered(), is exactly what plan §1 rule 8 forbids for a
// check's OWN behaviour.

// -------------------------------------------------- env.devcontainer-cli

func TestRunEnvDevcontainerCLI(t *testing.T) {
	for _, present := range []bool{false, true} {
		present := present
		t.Run("", func(t *testing.T) {
			env := NewMapEnv()
			if present {
				env.PathBinaries["devcontainer"] = "/opt/bin/devcontainer"
			}

			res := runEnvDevcontainerCLI(context.Background(), &Input{Env: env})

			if res.Outcome() != OutcomeFinding {
				t.Fatalf("present=%v: outcome %v, want finding", present, res.Outcome())
			}
			f := res.Findings[0]
			if f.Severity != SeverityInfo {
				t.Errorf("present=%v: severity %v, want info", present, f.Severity)
			}
			if present {
				if !strings.Contains(f.Detail, "/opt/bin/devcontainer") || f.Remedy != "" {
					t.Errorf("present=%v: got Detail=%q Remedy=%q, want path in Detail and no Remedy", present, f.Detail, f.Remedy)
				}
			} else if f.Remedy == "" {
				t.Errorf("present=%v: want a Remedy when devcontainer is absent", present)
			}
		})
	}
}

// -------------------------------------------------- env.no-backend-with-project

func TestRunEnvNoBackendWithProject(t *testing.T) {
	const workspace = "/home/user/project"

	tests := []struct {
		name        string
		markers     []string // subset of envProjectMarkers to create
		backend     string   // "" for none
		wantFinding bool
	}{
		{"no-markers-no-backend", nil, "", false},
		{"no-markers-with-backend", nil, "docker", false},
		{"devcontainer-marker-no-backend", []string{".devcontainer"}, "", true},
		{"manifest-marker-no-backend", []string{"manifest.scm"}, "", true},
		{"both-markers-no-backend", []string{".devcontainer", "manifest.scm"}, "", true},
		{"markers-with-guix", []string{"manifest.scm"}, "guix", false},
		{"markers-with-podman", []string{".devcontainer"}, "podman", false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			for _, m := range tc.markers {
				mode := fs.FileMode(0o644)
				if m == ".devcontainer" {
					mode = fs.ModeDir | 0o755
				}
				env.Files[workspace+"/"+m] = MapFile{Mode: mode}
			}
			if tc.backend != "" {
				env.PathBinaries[tc.backend] = "/opt/bin/" + tc.backend
			}

			res := runEnvNoBackendWithProject(context.Background(), &Input{Env: env, Workspace: workspace})

			if tc.wantFinding {
				if res.Outcome() != OutcomeFinding || len(res.Findings) != 1 {
					t.Fatalf("got outcome %v findings %+v, want exactly one finding", res.Outcome(), res.Findings)
				}
				f := res.Findings[0]
				if f.Severity != SeverityWarn {
					t.Errorf("severity = %v, want warn", f.Severity)
				}
				for _, m := range tc.markers {
					if !strings.Contains(f.Detail, m) {
						t.Errorf("Detail %q does not mention marker %q", f.Detail, m)
					}
				}
			} else if res.Outcome() != OutcomePass {
				t.Fatalf("got outcome %v, want pass", res.Outcome())
			}
		})
	}
}

// -------------------------------------------------- env.guix-runtime-selected

func TestRunEnvGuixRuntimeSelected(t *testing.T) {
	const workspace = "/home/user/project"

	tests := []struct {
		name        string
		guix        bool
		manifest    bool
		wantFinding bool
	}{
		{"neither", false, false, false},
		{"guix-only", true, false, false},
		{"manifest-only", false, true, false},
		{"both", true, true, true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env := NewMapEnv()
			if tc.guix {
				env.PathBinaries["guix"] = "/opt/bin/guix"
			}
			if tc.manifest {
				env.Files[workspace+"/manifest.scm"] = MapFile{Mode: 0o644}
			}

			res := runEnvGuixRuntimeSelected(context.Background(), &Input{Env: env, Workspace: workspace})

			if tc.wantFinding {
				if res.Outcome() != OutcomeFinding || len(res.Findings) != 1 {
					t.Fatalf("got outcome %v findings %+v, want exactly one finding", res.Outcome(), res.Findings)
				}
				if res.Findings[0].Severity != SeverityInfo {
					t.Errorf("severity = %v, want info", res.Findings[0].Severity)
				}
			} else if res.Outcome() != OutcomePass {
				t.Fatalf("got outcome %v, want pass", res.Outcome())
			}
		})
	}
}

// -------------------------------------------------------- env.xdg-writable

// envTestPaths is the HostPaths every TestRunEnvXDGWritable case shares.
var envTestPaths = HostPaths{
	ConfigDir:  "/home/user/.config/wraptool",
	DataDir:    "/home/user/.local/share/wraptool",
	RuntimeDir: "/run/user/1000/wraptool",
}

func TestRunEnvXDGWritable(t *testing.T) {
	t.Run("all writable, all exist, owner match", func(t *testing.T) {
		env := NewMapEnv()
		env.UID, env.GID = 1000, 1000
		for _, p := range []string{envTestPaths.ConfigDir, envTestPaths.DataDir, envTestPaths.RuntimeDir} {
			env.Files[p] = MapFile{Mode: fs.ModeDir | 0o700, UID: 1000, GID: 1000}
		}

		res := runEnvXDGWritable(context.Background(), &Input{Env: env, Paths: envTestPaths})

		if res.Outcome() != OutcomePass {
			t.Fatalf("outcome = %v, want pass; findings=%+v", res.Outcome(), res.Findings)
		}
	})

	t.Run("directories do not exist yet but the nearest ancestor is writable", func(t *testing.T) {
		env := NewMapEnv()
		env.UID, env.GID = 1000, 1000
		// Only the XDG parents exist (scaffold.go creates the leaf lazily).
		env.Files["/home/user/.config"] = MapFile{Mode: fs.ModeDir | 0o755, UID: 1000, GID: 1000}
		env.Files["/home/user/.local/share"] = MapFile{Mode: fs.ModeDir | 0o755, UID: 1000, GID: 1000}
		env.Files["/run/user/1000"] = MapFile{Mode: fs.ModeDir | 0o700, UID: 1000, GID: 1000}

		res := runEnvXDGWritable(context.Background(), &Input{Env: env, Paths: envTestPaths})

		if res.Outcome() != OutcomePass {
			t.Fatalf("outcome = %v, want pass; findings=%+v", res.Outcome(), res.Findings)
		}
	})

	t.Run("one unwritable directory yields exactly one finding naming it", func(t *testing.T) {
		env := NewMapEnv()
		env.UID, env.GID = 1000, 1000
		env.Files[envTestPaths.ConfigDir] = MapFile{Mode: fs.ModeDir | 0o500, UID: 1000, GID: 1000} // no owner write bit
		env.Files[envTestPaths.DataDir] = MapFile{Mode: fs.ModeDir | 0o700, UID: 1000, GID: 1000}
		env.Files[envTestPaths.RuntimeDir] = MapFile{Mode: fs.ModeDir | 0o700, UID: 1000, GID: 1000}

		res := runEnvXDGWritable(context.Background(), &Input{Env: env, Paths: envTestPaths})

		if res.Outcome() != OutcomeFinding || len(res.Findings) != 1 {
			t.Fatalf("got outcome %v findings %+v, want exactly one finding", res.Outcome(), res.Findings)
		}
		f := res.Findings[0]
		if f.Severity != SeverityFail {
			t.Errorf("severity = %v, want fail", f.Severity)
		}
		if !strings.Contains(f.Detail, envTestPaths.ConfigDir) {
			t.Errorf("Detail %q does not name the unwritable dir %q", f.Detail, envTestPaths.ConfigDir)
		}
	})

	t.Run("multiple unwritable directories yield one finding each", func(t *testing.T) {
		env := NewMapEnv()
		env.UID, env.GID = 1000, 1000
		env.Files[envTestPaths.ConfigDir] = MapFile{Mode: fs.ModeDir | 0o500, UID: 1000, GID: 1000}
		env.Files[envTestPaths.DataDir] = MapFile{Mode: fs.ModeDir | 0o500, UID: 1000, GID: 1000}
		env.Files[envTestPaths.RuntimeDir] = MapFile{Mode: fs.ModeDir | 0o700, UID: 1000, GID: 1000}

		res := runEnvXDGWritable(context.Background(), &Input{Env: env, Paths: envTestPaths})

		if len(res.Findings) != 2 {
			t.Fatalf("got %d findings, want 2: %+v", len(res.Findings), res.Findings)
		}
	})

	t.Run("writable via group bit, not owner", func(t *testing.T) {
		env := NewMapEnv()
		env.UID, env.GID = 1000, 2000
		env.Files[envTestPaths.ConfigDir] = MapFile{Mode: fs.ModeDir | 0o070, UID: 500, GID: 2000}
		env.Files[envTestPaths.DataDir] = MapFile{Mode: fs.ModeDir | 0o700, UID: 1000, GID: 1000}
		env.Files[envTestPaths.RuntimeDir] = MapFile{Mode: fs.ModeDir | 0o700, UID: 1000, GID: 1000}

		res := runEnvXDGWritable(context.Background(), &Input{Env: env, Paths: envTestPaths})

		if res.Outcome() != OutcomePass {
			t.Fatalf("outcome = %v, want pass (group-writable); findings=%+v", res.Outcome(), res.Findings)
		}
	})

	t.Run("writable via other bit, neither owner nor group match", func(t *testing.T) {
		env := NewMapEnv()
		env.UID, env.GID = 1000, 1000
		env.Files[envTestPaths.ConfigDir] = MapFile{Mode: fs.ModeDir | 0o007, UID: 500, GID: 500}
		env.Files[envTestPaths.DataDir] = MapFile{Mode: fs.ModeDir | 0o700, UID: 1000, GID: 1000}
		env.Files[envTestPaths.RuntimeDir] = MapFile{Mode: fs.ModeDir | 0o700, UID: 1000, GID: 1000}

		res := runEnvXDGWritable(context.Background(), &Input{Env: env, Paths: envTestPaths})

		if res.Outcome() != OutcomePass {
			t.Fatalf("outcome = %v, want pass (other-writable); findings=%+v", res.Outcome(), res.Findings)
		}
	})

	t.Run("unknown ownership falls back to any write bit", func(t *testing.T) {
		env := NewMapEnv()
		env.UID, env.GID = -1, -1 // Windows-shaped: NeedsModeBits still declared true here deliberately, since
		// the engine (not the check) is what gates GOOS==windows via provides(); this test exercises the
		// FALLBACK BRANCH of envDirWritable, not the Windows skip path itself.
		env.Files[envTestPaths.ConfigDir] = MapFile{Mode: fs.ModeDir | 0o444, UID: -1, GID: -1} // no write bit anywhere
		env.Files[envTestPaths.DataDir] = MapFile{Mode: fs.ModeDir | 0o666, UID: -1, GID: -1}   // other write bit set
		env.Files[envTestPaths.RuntimeDir] = MapFile{Mode: fs.ModeDir | 0o666, UID: -1, GID: -1}

		res := runEnvXDGWritable(context.Background(), &Input{Env: env, Paths: envTestPaths})

		if res.Outcome() != OutcomeFinding || len(res.Findings) != 1 {
			t.Fatalf("got outcome %v findings %+v, want exactly one finding (config dir only)", res.Outcome(), res.Findings)
		}
		if !strings.Contains(res.Findings[0].Detail, envTestPaths.ConfigDir) {
			t.Errorf("Detail %q does not name the unwritable dir", res.Findings[0].Detail)
		}
	})
}

func TestEnvNearestExistingAncestor(t *testing.T) {
	env := NewMapEnv()
	env.Files["/home/user"] = MapFile{Mode: fs.ModeDir | 0o755, UID: 1000, GID: 1000}

	fi, ok := envNearestExistingAncestor(env, "/home/user/.config/wraptool")
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if fi.Path != "/home/user" {
		t.Errorf("Path = %q, want /home/user", fi.Path)
	}

	if _, ok := envNearestExistingAncestor(NewMapEnv(), "/nowhere/at/all"); ok {
		t.Errorf("ok = true against an env with no root at all, want false")
	}
}

func TestEnvDirWritable(t *testing.T) {
	tests := []struct {
		name     string
		fi       FileInfo
		uid, gid int
		want     bool
	}{
		{"owner write bit, uid matches", FileInfo{Mode: 0o700, UID: 1000, GID: 1000}, 1000, 1000, true},
		{"no owner write bit, uid matches", FileInfo{Mode: 0o500, UID: 1000, GID: 1000}, 1000, 1000, false},
		{"group write bit, gid matches", FileInfo{Mode: 0o070, UID: 500, GID: 2000}, 1000, 2000, true},
		{"no group write bit, gid matches", FileInfo{Mode: 0o050, UID: 500, GID: 2000}, 1000, 2000, false},
		{"other write bit, neither matches", FileInfo{Mode: 0o007, UID: 500, GID: 500}, 1000, 1000, true},
		{"no other write bit, neither matches", FileInfo{Mode: 0o005, UID: 500, GID: 500}, 1000, 1000, false},
		{"unknown ownership, no write bit anywhere", FileInfo{Mode: 0o444, UID: -1, GID: -1}, 1000, 1000, false},
		{"unknown ownership, other write bit set", FileInfo{Mode: 0o446, UID: -1, GID: -1}, 1000, 1000, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := envDirWritable(tc.fi, tc.uid, tc.gid); got != tc.want {
				t.Errorf("envDirWritable(%+v, %d, %d) = %v, want %v", tc.fi, tc.uid, tc.gid, got, tc.want)
			}
		})
	}
}
