package cmd

import (
	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "wraptool",
		Short: "MCP server that wraps CLI tools with security whitelisting",
		Long: `wraptool is an MCP server that wraps CLI tools (git, gcloud, kubectl, etc.)
behind a security whitelist. AI coding assistants discover and use wrapped tools
via MCP, but never see credentials. Unapproved commands are denied with structured
responses, and capability requests are collected for admin review.

Run with no arguments in a git repository to bring up an isolated coding
environment (equivalent to 'wraptool up').`,
		SilenceUsage: true,
		// Bare `wraptool` in a repo → `up`; otherwise show help.
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := gitToplevel(); err != nil {
				return cmd.Help()
			}
			return runUp(upOptions{})
		},
	}

	root.AddCommand(newServeCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newRequestsCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newHarnessCmd())
	root.AddCommand(newUpCmd())
	root.AddCommand(newDownCmd())
	root.AddCommand(newServerCmd())

	return root
}
