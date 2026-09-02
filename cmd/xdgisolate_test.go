package cmd

import (
	"os"
	"testing"

	"github.com/adrg/xdg"
)

// withIsolatedXDG points one XDG env var at a temp dir and reloads xdg, so a
// test never touches the developer's real config/data/runtime dirs. Registers
// a t.Cleanup with the ordering all callers rely on: restore the env first,
// reload xdg second, in one cleanup. Not safe under t.Parallel() — it mutates
// process-wide env and adrg/xdg's package-level snapshot.
func withIsolatedXDG(t *testing.T, envVar string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	old, had := os.LookupEnv(envVar)
	if err := os.Setenv(envVar, dir); err != nil {
		t.Fatal(err)
	}
	xdg.Reload()
	t.Cleanup(func() {
		if had {
			os.Setenv(envVar, old)
		} else {
			os.Unsetenv(envVar)
		}
		xdg.Reload()
	})
	return dir
}
