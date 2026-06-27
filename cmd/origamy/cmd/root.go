package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "origamy",
	Short: "Origamy CLI — deploy and manage your data plane",
	Long: `Origamy CLI — deploy and manage your self-hosted data plane.`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(versionCmd)
}
