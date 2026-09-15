// Package cmd holds the cluster-manager CLI.
package cmd

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	version     = "dev"
	buildCommit = "unknown"
	buildDate   = "unknown"
)

// SetVersion records the build version (set from main via ldflags).
func SetVersion(v string) { version = v }

// SetBuildInfo records the commit and build date.
func SetBuildInfo(commit, date string) {
	buildCommit = commit
	buildDate = date
}

func newRootCmd() *cobra.Command {
	var verbose bool
	root := &cobra.Command{
		Use:   "cluster-manager",
		Short: "The Agent Platform's MCP-only cluster write surface",
		Long: `cluster-manager exposes the installation's clusters and their node pools as
MCP tools: list_clusters, list_node_pools and get_info today; the node-pool
writes (create_node_pool, delete_node_pool, enable/disable_model_serving),
each with dryRun and mode apply|commit, follow. Every Kubernetes call is made
as the caller: muster forwards the session's IdP token and this server
presents it to the API server, so the caller's RBAC governs.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	root.Version = version
	root.SetVersionTemplate("cluster-manager version {{.Version}}\n")
	root.AddCommand(newServeCmd(), newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "cluster-manager version %s\n  commit: %s\n  built:  %s\n", version, buildCommit, buildDate)
		},
	}
}

// Execute runs the CLI.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
