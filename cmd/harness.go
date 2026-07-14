package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/pti/wraptool/internal/harness"

	"github.com/spf13/cobra"
)

func newHarnessCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "harness",
		Short: "Manage the developer harness pool (binaries shared into the guix container)",
		Long: `The harness pool is a developer-scoped directory of AI-harness binaries shared
into every guix container, so a harness is installed once across all projects
(not per session). It lives at $XDG_DATA_HOME/wraptool/harness-pool (bin/ on
PATH, home/ as the harness HOME). This is the developer's toolbox — not the
project (manifest.scm) and not host credentials.`,
	}
	cmd.AddCommand(newHarnessListCmd(), newHarnessInstallCmd())
	return cmd
}

func newHarnessListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List known harnesses and their pool install status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, poolBin, _, err := ensureHarnessPool()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "HARNESS\tKIND\tINSTALLED\tAUTH")
			for _, name := range harness.Names() {
				d := harness.Registry[name]
				kind, installed := "host IDE", "-"
				if d.IsCLI() {
					kind = "container CLI"
					switch {
					case fileExists(filepath.Join(poolBin, d.Bin)):
						installed = "yes"
					case len(d.Install) > 0:
						installed = "no (installable)"
					default:
						installed = "no (manual)"
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, kind, installed, authLabel(d.Auth))
			}
			return w.Flush()
		},
	}
}

func newHarnessInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install <harness>",
		Short: "Install a harness binary into the pool",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			d, ok := harness.Registry[name]
			if !ok {
				return fmt.Errorf("unknown harness %q; supported: %v", name, harness.Names())
			}
			if !d.IsCLI() {
				return fmt.Errorf("%q is a host-side IDE/extension, not a container CLI; "+
					"configure it with `wraptool init --harness %s`", name, name)
			}
			pool, poolBin, _, err := ensureHarnessPool()
			if err != nil {
				return err
			}
			if len(d.Install) == 0 {
				return fmt.Errorf("no automated installer for %q; install it into %s yourself, "+
					"then `wraptool up --harness %s`", name, poolBin, name)
			}

			argv := make([]string, len(d.Install))
			for i, a := range d.Install {
				argv[i] = strings.ReplaceAll(a, "{prefix}", pool)
			}
			// The pool installers run the harness's own installer inside a guix
			// container (FHS emulation dodges npm's detect-libc misread on bare
			// guix). Off guix — a devcontainer host — the pool isn't the delivery
			// path at all: the wraptool-harness dev-container feature installs the
			// binary per image and shares state via the pool home mount. Point there
			// instead of failing on a missing `guix`.
			if argv[0] == "guix" && !hasGuix() {
				return fmt.Errorf("`wraptool harness install` provisions the guix-container pool, "+
					"but `guix` is not on PATH.\nOn a devcontainer host, install %q via the "+
					"wraptool-harness feature instead: add\n  "+
					`"forge.snamellit.com/pti/wraptool/wraptool-harness:0": {"harness": %q}`+
					"\nto your .devcontainer/devcontainer.json (see examples/devcontainer). "+
					"`wraptool up` shares the pool home so auth/skills persist across projects", name, name)
			}
			fmt.Printf("installing %s into %s ...\n  %s\n", name, poolBin, strings.Join(argv, " "))
			c := exec.Command(argv[0], argv[1:]...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("installing %s: %w", name, err)
			}

			if bin := filepath.Join(poolBin, d.Bin); fileExists(bin) {
				fmt.Printf("installed %s -> %s\n", name, bin)
			} else {
				fmt.Printf("warning: %q not found after install (expected %s)\n", d.Bin, bin)
			}
			return nil
		},
	}
}

func authLabel(a harness.AuthMode) string {
	switch a {
	case harness.AuthHeader:
		return "bearer"
	case harness.AuthNone:
		return "none"
	case harness.AuthUnverified:
		return "unverified"
	default:
		return "unverified"
	}
}
