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
responses, and capability requests are collected for admin review.`,
	}

	root.AddCommand(newServeCmd())
	root.AddCommand(newRequestsCmd())
	root.AddCommand(newInitCmd())

	return root
}
