package doctor

import (
	"context"
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
			for _, b := range envBackends {
				if want[b] && !strings.Contains(f.Detail, b) {
					t.Errorf("mask %03b: detail %q missing present backend %q", mask, f.Detail, b)
				}
				if !want[b] && !strings.Contains(f.Detail, b) {
					t.Errorf("mask %03b: detail %q missing absent backend %q", mask, f.Detail, b)
				}
			}
		})
	}
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
