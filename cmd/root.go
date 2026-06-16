package cmd

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var rootCmd = &cobra.Command{
	Use:   "atc",
	Short: "Make live a bit easier by automatically creating consul service-resolver config",
	Long: `Like with actual airports we sometimes need a process that controls what should happen with ingress requests. 
manually setting up failover and redirect consul service-resolver config can be quite laborious.`,
}

func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	viper.SetEnvPrefix("atc")
	viper.AutomaticEnv()

	viper.SetDefault("log_level", "info")
}
