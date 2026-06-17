package cmd

import (
	"fmt"
	"os"
	"slices"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/atcprojectio/atc/pkg/atc"
)

var modulesCmd = &cobra.Command{
	Use:   "modules",
	Short: "List available values that can be used as target.",
	Long:  "List available values that can be used as target.",
	Run: func(cmd *cobra.Command, args []string) {
		logLevel := viper.GetString("log_level")
		cfg := atc.Config{}
		cfg.Server.LogLevel = logLevel
		t, _ := atc.New(cfg)
		allDeps := t.DependenciesForModule(atc.All)

		for _, m := range t.UserVisibleModuleNames() {
			included := slices.Contains(allDeps, m)

			if included {
				fmt.Fprintln(os.Stdout, m, "*")
			} else {
				fmt.Fprintln(os.Stdout, m)
			}
		}

		fmt.Fprintln(os.Stdout)
		fmt.Fprintln(os.Stdout, "Modules marked with * are included in target All.")
	},
}

func init() {
	rootCmd.AddCommand(modulesCmd)
}
