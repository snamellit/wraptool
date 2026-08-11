package cmd

import (
	"fmt"

	"github.com/pti/wraptool/internal/buildinfo"

	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "wraptool",
		Version: buildinfo.Version(), // `wraptool --version`
		Short:   "MCP server that wraps CLI tools with security whitelisting",
		Long: `wraptool is an MCP server that wraps CLI tools (git, gcloud, kubectl, etc.)
behind a security whitelist. AI coding assistants discover and use wrapped tools
via MCP, but never see credentials. Unapproved commands are denied with structured
responses, and capability requests are collected for admin review.

Run with no arguments in a git repository to bring up an isolated coding
environment (equivalent to 'wraptool up').`,
		SilenceUsage: true,
		// main.go prints the returned error itself; without this cobra
		// prints it too and every failure shows up twice.
		SilenceErrors: true,
		// Refuse root once, at the root command, so it covers every
		// subcommand including any added later. cobra runs only the most
		// specific PersistentPreRunE it finds, so no subcommand may define
		// one of its own without re-invoking refuseRoot.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return refuseRoot()
		},
		// Bare `wraptool` in a repo → `up`; otherwise show help.
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := gitToplevel(); err != nil {
				return cmd.Help()
			}
			return runUp(defaultUpOptions())
		},
	}

	root.AddCommand(newServeCmd("serve", true)) // hidden alias for `server start`
	root.AddCommand(newConfigCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newRequestsCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newHarnessCmd())
	root.AddCommand(newUpCmd())
	root.AddCommand(newDownCmd())
	root.AddCommand(newServerCmd())
	root.AddCommand(newVersionCmd())

	return root
}

// newVersionCmd prints the full build identity — version, git revision, dirty
// flag, Go version — so you can check any wraptool binary (including a stale one
// on PATH) without starting a server. `which wraptool && wraptool version`
// answers "which binary am I running?".
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the wraptool build version and git revision",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			i := buildinfo.Get()
			fmt.Printf("wraptool %s\n", i.Version)
			if i.Revision != "" {
				fmt.Printf("  revision: %s (dirty: %v)\n", i.Revision, i.Modified)
			}
			fmt.Printf("  go:       %s\n", i.GoVersion)
			return nil
		},
	}
}
