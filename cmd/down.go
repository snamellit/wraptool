package cmd

import (
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
		Short: "Stop this project's dev container",
		Long: `Stop the dev container for the current project (the counterpart to ` + "`up`" + `).

Finds the container by its dev-container workspace label and stops it. The shared
wraptool server is left running (it serves other projects/worktrees) — stop it
separately with ` + "`wraptool server stop`" + `.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDown(workspace, remove)
		},
	}

	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace root (default: the git worktree root of the current directory)")
	cmd.Flags().BoolVar(&remove, "rm", false, "remove the container as well, not just stop it (forces a fresh create + postCreate on next up)")

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

	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not found; needed to stop the dev container")
	}

	ids, err := devcontainerIDs(root)
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

	c := exec.Command("docker", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", dockerCmd, err)
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
