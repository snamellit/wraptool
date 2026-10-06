package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newDownCmd() *cobra.Command {
	var (
		workspace string
		remove    bool
	)

	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop this project's sandbox and dev container",
		Long: `Stop what ` + "`up`" + ` started for the current project (the counterpart to ` + "`up`" + `).

Looks for a Docker Sandboxes sandbox (by its name, when ` + "`sbx`" + ` is on PATH) and for
dev containers (by their dev-container workspace label, when ` + "`docker`" + ` is on PATH)
and stops whatever exists. The shared wraptool server is left running (it serves
other projects/worktrees) — stop it separately with ` + "`wraptool server stop`" + `.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDown(workspace, remove)
		},
	}

	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace root (default: the git worktree root of the current directory)")
	cmd.Flags().BoolVar(&remove, "rm", false, "remove the sandbox/container as well, not just stop it (forces a fresh create on next up)")

	return cmd
}

func runDown(workspace string, remove bool) error {
	root := workspace
	if root == "" {
		var err error
		root, err = gitToplevel()
		if err != nil {
			return fmt.Errorf("not a git repository (run in a repo/worktree root, or pass --workspace): %w", err)
		}
	}
	root, _ = filepath.Abs(root)

	sbx, docker := hasSbx(), hasDocker()
	if !sbx && !docker {
		return errors.New("neither `sbx` nor `docker` found on PATH; there is nothing `down` can stop")
	}

	// Each side runs regardless of the other's outcome: a failed sandbox stop
	// must not leave a dev container running, and vice versa.
	var errs []error
	if sbx {
		if err := downSandbox(root, remove); err != nil {
			errs = append(errs, err)
		}
	}
	if docker {
		if err := downDevContainers(root, remove); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// hasDocker reports whether the docker CLI is on PATH. A var, like hasGuix, so
// tests stub it.
var hasDocker = func() bool { _, err := exec.LookPath("docker"); return err == nil }

// dockerRunner is the process boundary for the two docker calls down makes.
type dockerRunner interface {
	// PS lists the IDs of dev containers labelled with root (running and stopped).
	PS(root string) ([]string, error)
	// Run executes `docker <args...>` with output passed through.
	Run(args []string) error
}

var dockerRun dockerRunner = execDockerRunner{}

type execDockerRunner struct{}

func (execDockerRunner) PS(root string) ([]string, error) { return devcontainerIDs(root) }

func (execDockerRunner) Run(args []string) error {
	c := exec.Command("docker", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func downDevContainers(root string, remove bool) error {
	ids, err := dockerRun.PS(root)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Printf("no dev container found for %s\n", root)
		return nil
	}

	verb, dockerCmd := "stopping", "stop"
	if remove {
		verb, dockerCmd = "removing", "rm"
	}
	fmt.Printf("%s dev container(s) for %s: %s\n", verb, root, strings.Join(ids, " "))

	args := []string{dockerCmd}
	if remove {
		args = append(args, "-f") // rm -f stops then removes
	}
	args = append(args, ids...)

	if err := dockerRun.Run(args); err != nil {
		return fmt.Errorf("docker %s: %w", dockerCmd, err)
	}
	return nil
}

// downSandbox stops (or, with remove, deletes) this workspace's sandbox and the
// wraptool-side state that belongs to it.
func downSandbox(root string, remove bool) error {
	name := sbxSandboxName(root)

	out, errOut, err := sbxCapture([]string{"ls", "--quiet"})
	if err != nil {
		return fmt.Errorf("running `sbx ls --quiet`: %w: %s", err, strings.TrimSpace(errOut+out))
	}
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			found = true
			break
		}
	}

	// Read before any removal: the pinhole resource lives only in this file.
	st, haveState, err := readSbxState(name)
	if err != nil {
		return fmt.Errorf("reading sandbox state for %s: %w", name, err)
	}

	if !found {
		if haveState && remove {
			if err := removeSbxState(name); err != nil {
				return fmt.Errorf("removing stale sandbox state for %s: %w", name, err)
			}
			fmt.Printf("no sandbox found for %s; removed stale state for %s\n", root, name)
			return nil
		}
		fmt.Printf("no sandbox found for %s\n", root)
		return nil
	}

	if !remove {
		fmt.Printf("stopping sandbox %s for %s\n", name, root)
		return sbxRunChecked([]string{"stop", name})
	}

	fmt.Printf("removing sandbox %s for %s\n", name, root)
	if err := sbxRunChecked([]string{"rm", "--force", name}); err != nil {
		return err // state kept: the sandbox still exists
	}
	if err := removeSbxState(name); err != nil {
		return fmt.Errorf("removing sandbox state for %s: %w", name, err)
	}
	// Whether `sbx rm` already drops sandbox-scoped policy rules is unconfirmed
	// (spec S8), so removal is best-effort and never fails down.
	if haveState && st.PinholeResource != "" {
		argv := []string{"policy", "rm", "network", "--sandbox", name, "--resource", st.PinholeResource, "--force"}
		if err := sbxRunChecked(argv); err != nil {
			fmt.Printf("notice: could not remove the network rule for %s (it may already be gone): %v\n", st.PinholeResource, err)
		}
	}
	return nil
}

// sbxRunChecked runs one sbx command through the seam, echoing its output and
// wrapping a failure with the command line and stderr.
func sbxRunChecked(argv []string) error {
	out, errOut, err := sbxCapture(argv)
	if s := strings.TrimSpace(out); s != "" {
		fmt.Println(s)
	}
	if err != nil {
		return fmt.Errorf("running `sbx %s`: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(errOut))
	}
	return nil
}

// devcontainerIDs returns the container IDs whose dev-container workspace label
// matches root (running and stopped).
func devcontainerIDs(root string) ([]string, error) {
	out, err := exec.Command("docker", "ps", "-a",
		"--filter", "label=devcontainer.local_folder="+root,
		"--format", "{{.ID}}").Output()
	if err != nil {
		return nil, fmt.Errorf("listing dev containers: %w", err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids, nil
}
