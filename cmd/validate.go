package cmd

import (
	"fmt"
	"os"

	"github.com/pti/wraptool/internal/config"
	"github.com/pti/wraptool/internal/paths"

	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate wraptool configuration",
	}
	cmd.AddCommand(newConfigValidateCmd())
	return cmd
}

func newConfigValidateCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "validate [path]",
		Short: "Validate a config file without starting the server",
		Long: `Validate a wraptool config file without starting the server or spawning
any wrapped tools.

Unlike the lenient load the server performs at runtime, validation strictly
decodes the YAML: unknown or misspelled keys (e.g. flag_constraint instead of
flag_constraints) are reported rather than silently ignored. It then applies
defaults and runs full semantic validation, exiting non-zero with a message on
the first problem.

The config path is given as either the positional argument or --config (not
both); if neither is set it falls back to $WRAPTOOL_CONFIG, then the default
location.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && cmd.Flags().Changed("config") {
				return fmt.Errorf("pass the config path as either the positional argument or --config, not both")
			}
			path := configPath
			if len(args) == 1 {
				path = args[0]
			}
			if path == "" {
				path = os.Getenv("WRAPTOOL_CONFIG")
			}
			if path == "" {
				path = paths.DefaultConfigFile()
			}
			return runConfigValidate(cmd, path)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to config file (default: "+paths.DefaultConfigFile()+")")

	return cmd
}

func runConfigValidate(cmd *cobra.Command, path string) error {
	cfg, err := config.LoadStrict(path)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validating config: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "ok: %s is valid — %d tool(s), %d mcp server(s)\n",
		path, len(cfg.Tools), len(cfg.MCPServers))
	return nil
}
