package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "origamy",
	Short: "Origamy CLI — deploy and manage your data plane",
	Long:  `Origamy CLI — deploy and manage your self-hosted data plane.`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		// A blank message means the command already printed a styled,
		// actionable error (see deploy.go's fail/diagnose). Just exit non-zero.
		if msg := err.Error(); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(uninstallCmd)
	rootCmd.AddCommand(versionCmd)
}
