package cmd

import (
	"fmt"

	"github.com/prometheus/common/version"
	"github.com/spf13/cobra"
)

// versionCmd represents the version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Return the version identifier.",
	Long:  `Return the version identifier for this application.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("atc %s, commit %s, built at %s\n\n", version.Version, version.Revision, version.BuildDate)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
