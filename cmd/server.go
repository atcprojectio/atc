package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/atcprojectio/atc/pkg/atc"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start as a background process.",
	Long:  "Start as a background process.",
	RunE: func(cmd *cobra.Command, args []string) error {
		var cfg atc.Config
		configFile := viper.GetString("config")
		if configFile != "" {
			viper.SetConfigFile(configFile)
			if err := viper.ReadInConfig(); err != nil {
				return fmt.Errorf("failed to read config file %s: %w", configFile, err)
			}
		}

		if err := viper.Unmarshal(&cfg); err != nil {
			return fmt.Errorf("failed to unmarshal configuration: %w", err)
		}

		if cfg.Server.HTTPListenPort == 0 {
			cfg.Server.HTTPListenPort = viper.GetInt("port")
		}
		if cfg.Server.MetricsListenPort == 0 {
			cfg.Server.MetricsListenPort = viper.GetInt("metrics_port")
		}
		if len(cfg.Target) == 0 {
			cfg.Target = viper.GetStringSlice("target")
		}
		if cfg.Server.LogLevel == "" {
			cfg.Server.LogLevel = viper.GetString("log_level")
		}
		if cfg.ConsulAddr == "" {
			cfg.ConsulAddr = viper.GetString("consul_addr")
		}
		if cfg.ConsulToken == "" {
			cfg.ConsulToken = viper.GetString("consul_token")
		}
		if cfg.ConsulDC == "" {
			cfg.ConsulDC = viper.GetString("consul_dc")
		}
		cfg.Server.MetricsNamespace = "atc"

		t, err := atc.New(cfg)
		if err != nil {
			return err
		}

		return t.Run()
	},
}

func init() {
	rootCmd.AddCommand(serverCmd)

	serverCmd.PersistentFlags().Int("port", 8088, "port to expose service on.")
	_ = viper.BindPFlag("port", serverCmd.PersistentFlags().Lookup("port"))

	serverCmd.PersistentFlags().Int("metrics_port", 8089, "port to expose metrics on.")
	_ = viper.BindPFlag("metrics_port", serverCmd.PersistentFlags().Lookup("metrics_port"))

	serverCmd.PersistentFlags().StringSlice("target", []string{"all"}, "Comma-separated list of components to include in the instantiated process. Use the 'modules' command line flag to get a list of available components, and to see which components are included with 'all'. (default all)")
	_ = viper.BindPFlag("target", serverCmd.PersistentFlags().Lookup("target"))

	serverCmd.PersistentFlags().String("log_level", "info", "Only log messages with the given severity or above.")
	_ = viper.BindPFlag("log_level", serverCmd.PersistentFlags().Lookup("log_level"))

	serverCmd.PersistentFlags().String("consul_addr", "", "Consul HTTP address.")
	_ = viper.BindPFlag("consul_addr", serverCmd.PersistentFlags().Lookup("consul_addr"))

	serverCmd.PersistentFlags().String("consul_token", "", "Consul ACL token.")
	_ = viper.BindPFlag("consul_token", serverCmd.PersistentFlags().Lookup("consul_token"))

	serverCmd.PersistentFlags().String("consul_dc", "", "Consul datacenter.")
	_ = viper.BindPFlag("consul_dc", serverCmd.PersistentFlags().Lookup("consul_dc"))

	serverCmd.PersistentFlags().String("config", "", "Path to ATC configuration file.")
	_ = viper.BindPFlag("config", serverCmd.PersistentFlags().Lookup("config"))
}
