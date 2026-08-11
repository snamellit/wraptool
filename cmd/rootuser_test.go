package cmd

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRefuseRootForUID(t *testing.T) {
	for _, tc := range []struct {
		name string
		euid int
		want bool
	}{
		{"root", 0, true},
		{"ordinary user", 1000, false},
		{"windows unknown uid", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseRootFor(tc.euid)
			if got := err != nil; got != tc.want {
				t.Fatalf("refuseRootFor(%d) error = %v, want error: %v", tc.euid, err, tc.want)
			}
			if !tc.want {
				return
			}
			if !errors.Is(err, errRunningAsRoot) {
				t.Errorf("refuseRootFor(0) = %v, which does not wrap errRunningAsRoot", err)
			}
			// The message is this check's entire product: it must say what
			// wraptool will not do and what the operator should do instead.
			for _, want := range []string{"refuses to run as root", "wrapped tool", "Run wraptool as the user"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal message %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestRefuseRootReadsTheEffectiveUID guards the wiring between the exported
// helper and its testable core.
func TestRefuseRootReadsTheEffectiveUID(t *testing.T) {
	err := refuseRoot()
	if (err != nil) != (os.Geteuid() == 0) {
		t.Fatalf("refuseRoot() = %v while running as euid %d", err, os.Geteuid())
	}
}

// TestRootCommandRefusesRootBeforeAnySubcommandRuns pins the wiring: the guard
// lives on the root command so every subcommand inherits it, including ones
// added later. cobra runs only the MOST SPECIFIC PersistentPreRunE it finds, so
// a subcommand defining its own would silently bypass the refusal.
func TestRootCommandRefusesRootBeforeAnySubcommandRuns(t *testing.T) {
	root := NewRootCmd()
	if root.PersistentPreRunE == nil {
		t.Fatal("root command has no PersistentPreRunE; the root refusal is not wired")
	}
	for _, c := range root.Commands() {
		if c.PersistentPreRunE != nil {
			t.Errorf("subcommand %q defines its own PersistentPreRunE, which overrides the root refusal; call refuseRoot() from it too", c.Name())
		}
	}
}
