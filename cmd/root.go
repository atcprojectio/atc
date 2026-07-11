package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

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
	if err := runRoot(); err != nil {
		os.Exit(1)
	}
}

func runRoot() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return rootCmd.ExecuteContext(ctx)
}

func init() {
	viper.SetEnvPrefix("atc")
	viper.AutomaticEnv()

	viper.SetDefault("log_level", "info")

	rootCmd.PersistentFlags().String("config", "", "Path to ATC configuration file.")
	_ = viper.BindPFlag("config", rootCmd.PersistentFlags().Lookup("config"))
}
