package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "origamy",
	Short: "Origamy CLI — deploy and manage your data plane",
	Long: `
   ___  ____  ___ ____  ___  __  ____  _  _
  / _ \|  _ \|_ _/ ___|/ _ \|  \/  \ \/ /
 | | | | |_) || | |  _| | | | |\/| |\  /
 | |_| |  _ < | | |_| | |_| | |  | |/  \
  \___/|_| \_\___\____|\___/|_|  |_/_/\_\

  Customer Data Platform — self-hosted data plane`,
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
