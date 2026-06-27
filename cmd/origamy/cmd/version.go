package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// BuildVersion is set at link time: -ldflags "-X ...cmd.BuildVersion=v0.1.0"
var BuildVersion = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the CLI version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("origamy %s\n", BuildVersion)
	},
}
