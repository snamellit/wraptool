package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/paths"
	"github.com/pti/wraptool/internal/requestlog"

	"github.com/spf13/cobra"
)

func newRequestsCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "requests",
		Short: "Manage capability requests",
	}

	cmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "path to config file")

	cmd.AddCommand(newRequestsListCmd(&configPath))
	cmd.AddCommand(newRequestsApproveCmd(&configPath))
	cmd.AddCommand(newRequestsDenyCmd(&configPath))

	return cmd
}

func getStore(configPath *string) (*requestlog.Store, error) {
	path := *configPath
	if path == "" {
		path = os.Getenv("WRAPTOOL_CONFIG")
	}
	if path == "" {
		path = paths.DefaultConfigFile()
	}

	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}

	return requestlog.NewStore(cfg.Server.RequestLog), nil
}

func newRequestsListCmd(configPath *string) *cobra.Command {
	var statusFilter string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List capability requests",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := getStore(configPath)
			if err != nil {
				return err
			}

			requests, err := store.List(requestlog.Status(statusFilter))
			if err != nil {
				return err
			}

			if len(requests) == 0 {
				fmt.Println("No requests found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tTOOL\tSUBCOMMAND\tFLAGS\tSTATUS\tRATIONALE")
			for _, r := range requests {
				rationale := r.Rationale
				if len(rationale) > 60 {
					rationale = rationale[:57] + "..."
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					r.ID,
					r.Tool,
					strings.Join(r.Subcommand, " "),
					strings.Join(r.Flags, " "),
					r.Status,
					rationale,
				)
			}
			return w.Flush()
		},
	}

	cmd.Flags().StringVar(&statusFilter, "status", "pending", "filter by status (pending|approved|denied|\"\")")

	return cmd
}

func newRequestsApproveCmd(configPath *string) *cobra.Command {
	var apply bool

	cmd := &cobra.Command{
		Use:   "approve <request-id>",
		Short: "Approve a capability request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := getStore(configPath)
			if err != nil {
				return err
			}

			req, err := store.Get(args[0])
			if err != nil {
				return err
			}

			if err := store.UpdateStatus(args[0], requestlog.StatusApproved, ""); err != nil {
				return err
			}
			fmt.Printf("Request %s approved.\n", args[0])

			if !apply {
				fmt.Println("Note: manually add the capability to your config.yaml; a running wraptool will hot-reload on save (or send SIGHUP).")
				return nil
			}

			resolvedPath := *configPath
			if resolvedPath == "" {
				resolvedPath = os.Getenv("WRAPTOOL_CONFIG")
			}
			if resolvedPath == "" {
				resolvedPath = paths.DefaultConfigFile()
			}

			result, err := config.ApplyRequest(resolvedPath, req.Tool, req.Subcommand, req.Flags, req.FlagTypes)
			if err != nil {
				return fmt.Errorf("applying to config: %w", err)
			}

			switch {
			case result.NewRule:
				fmt.Printf("Added new allow rule to %s for %s %s:\n", resolvedPath, req.Tool, strings.Join(req.Subcommand, " "))
			case len(result.AddedFlags) > 0:
				fmt.Printf("Extended existing allow rule in %s with flags %v:\n", resolvedPath, result.AddedFlags)
			default:
				fmt.Printf("No change required; %s %s with flags %v is already allowed.\n", req.Tool, strings.Join(req.Subcommand, " "), req.Flags)
				return nil
			}
			if result.Snippet != "" {
				fmt.Println()
				fmt.Print(result.Snippet)
			}
			fmt.Println("\nA running wraptool will pick this up automatically via file-watch reload. Auto-added flags have no `flag_constraints` — tighten them if needed.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&apply, "apply", false, "write the approved capability into the config file (atomic; triggers hot-reload)")

	return cmd
}

func newRequestsDenyCmd(configPath *string) *cobra.Command {
	var reason string

	cmd := &cobra.Command{
		Use:   "deny <request-id>",
		Short: "Deny a capability request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := getStore(configPath)
			if err != nil {
				return err
			}

			if err := store.UpdateStatus(args[0], requestlog.StatusDenied, reason); err != nil {
				return err
			}

			fmt.Printf("Request %s denied.\n", args[0])
			return nil
		},
	}

	cmd.Flags().StringVar(&reason, "reason", "", "reason for denial")

	return cmd
}
