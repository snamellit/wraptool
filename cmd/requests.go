package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/pti/wraptool/internal/adminops"
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

// resolveConfigPath applies the standard --config / WRAPTOOL_CONFIG / default
// fallback shared by every requests subcommand.
func resolveConfigPath(configPath *string) string {
	path := *configPath
	if path == "" {
		path = os.Getenv("WRAPTOOL_CONFIG")
	}
	if path == "" {
		path = paths.DefaultConfigFile()
	}
	return path
}

func getStore(configPath *string) (*requestlog.Store, error) {
	cfg, err := config.Load(resolveConfigPath(configPath))
	if err != nil {
		return nil, err
	}

	return requestlog.NewStore(cfg.MCP.RequestLog), nil
}

func newRequestsListCmd(configPath *string) *cobra.Command {
	var statusFilter string
	var asJSON bool

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

			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(requests)
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
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the requests as a JSON array instead of a table")

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

			// Fetched here (adminops.Approve fetches it again internally) purely to
			// read Tool/Subcommand/Flags/FlagTypes for the messages below.
			req, err := store.Get(args[0])
			if err != nil {
				return err
			}

			resolvedPath := resolveConfigPath(configPath)

			result, err := adminops.Approve(store, resolvedPath, args[0], apply, req.FlagTypes)
			if err != nil {
				return err
			}
			fmt.Printf("Request %s approved.\n", args[0])

			if result.Outcome == adminops.OutcomeNotApplied {
				fmt.Println("Note: manually add the capability to your config.yaml; a running wraptool will hot-reload on save (or send SIGHUP).")
				return nil
			}

			if result.ApplyErr != nil {
				return fmt.Errorf("applying to config: %w", result.ApplyErr)
			}

			switch result.Outcome {
			case adminops.OutcomeNewRule:
				fmt.Printf("Added new allow rule to %s for %s %s:\n", resolvedPath, req.Tool, strings.Join(req.Subcommand, " "))
			case adminops.OutcomeExtended:
				fmt.Printf("Extended existing allow rule in %s with flags %v:\n", resolvedPath, result.AddedFlags)
			case adminops.OutcomeNoChange:
				fmt.Printf("No change required; %s %s with flags %v is already allowed.\n", req.Tool, strings.Join(req.Subcommand, " "), req.Flags)
				return nil
			case adminops.OutcomeNotApplied:
				// unreachable: handled above
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

			if err := adminops.Deny(store, args[0], reason); err != nil {
				return err
			}

			fmt.Printf("Request %s denied.\n", args[0])
			return nil
		},
	}

	cmd.Flags().StringVar(&reason, "reason", "", "reason for denial")

	return cmd
}
