package doctor

import (
	"context"
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

			// Assert on the rendered SEGMENTS, not mere substring presence: a
			// regression that swapped envDescribeBackends' present/absent
			// arguments (reporting a found backend as "not found" and vice
			// versa) must fail this test. found/notFound below are the exact
			// "found: "/"not found: " segments of Detail.
			found, notFound := splitBackendDetail(t, f.Detail)
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

// splitBackendDetail parses envDescribeBackends' rendered Detail back into its
// "found: a, b" and "not found: c" backend lists, so the test can assert each
// backend landed on the correct side rather than merely appearing somewhere in
// the string.
func splitBackendDetail(t *testing.T, detail string) (found, notFound []string) {
	t.Helper()
	for _, seg := range strings.Split(detail, "; ") {
		switch {
		case strings.HasPrefix(seg, "found: "):
			found = parseBackendNames(strings.TrimPrefix(seg, "found: "))
		case strings.HasPrefix(seg, "not found: "):
			notFound = parseBackendNames(strings.TrimPrefix(seg, "not found: "))
		case seg == "none found on PATH":
			// no present backends; found stays nil
		default:
			t.Fatalf("unrecognized Detail segment %q in %q", seg, detail)
		}
	}
	return found, notFound
}

// parseBackendNames strips each entry's " (path)" suffix (present backends
// are rendered as "guix (/opt/bin/guix)") to recover the bare name.
func parseBackendNames(list string) []string {
	parts := strings.Split(list, ", ")
	out := make([]string, len(parts))
	for i, p := range parts {
		name, _, _ := strings.Cut(p, " (")
		out[i] = name
	}
	return out
}

func TestEnvContainerBackendRegistered(t *testing.T) {
	found := false
	for _, c := range Registered() {
		if c.Meta().ID == envContainerBackend {
			found = true
			if c.Meta().Needs != NeedsNothing {
				t.Errorf("env.container-backend must need nothing, got %v", c.Meta().Needs)
			}
		}
	}
	if !found {
		t.Fatal("env.container-backend is not registered")
	}
}
