// Package cli provides the Cobra command-line interface.
package cli

import (
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/somaz94/plainbucket/internal/config"
	"github.com/somaz94/plainbucket/internal/server"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "plainbucket",
	Short: "S3-compatible object server that stores objects as plain files",
	Long: "plainbucket serves the S3 API from a directory: bucket <b> is the directory <root>/<b> " +
		"and object <key> is the file <root>/<b>/<key>, readable without the server.",
	RunE:          runServe,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "path to YAML config file")
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx, cfg)
}
