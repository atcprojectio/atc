package cmd

import (
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/attachmentgenie/atc/pkg/atc"
	atc_server "github.com/attachmentgenie/atc/pkg/atc/server"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start as a background process.",
	Long:  "Start as a background process.",
	RunE: func(cmd *cobra.Command, args []string) error {
		port := viper.GetInt("port")
		metricsPort := viper.GetInt("metrics_port")
		target := viper.GetStringSlice("target")
		logLevel := viper.GetString("log_level")
		consulAddr := viper.GetString("consul_addr")
		consulToken := viper.GetString("consul_token")
		consulDC := viper.GetString("consul_dc")

		cfg := atc.Config{
			Server: atc_server.Config{
				HTTPListenPort:    port,
				MetricsListenPort: metricsPort,
				MetricsNamespace:  "atc",
				LogLevel:          logLevel,
			},
			Target:      target,
			ConsulAddr:  consulAddr,
			ConsulToken: consulToken,
			ConsulDC:    consulDC,
		}

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
}
