package cmd

import (
	"errors"
	"fmt"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/atcprojectio/atc/pkg/atc"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start as a background process.",
	Long:  "Start as a background process.",
	RunE: func(cmd *cobra.Command, args []string) error {
		configFile := viper.GetString("config")
		if configFile != "" {
			viper.SetConfigFile(configFile)
		}
		load := func() (atc.Config, error) { return loadServerConfig(cmd) }

		cfg, err := load()
		if err != nil {
			return err
		}
		if errs := atc.ValidateConfig(cfg); len(errs) > 0 {
			return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
		}

		t, err := atc.New(cfg)
		if err != nil {
			return err
		}
		// Reloads (file watcher, API, MCP) use the same loader, so flags keep precedence.
		t.SetConfigLoader(load)

		if configFile != "" {
			viper.OnConfigChange(func(e fsnotify.Event) {
				// Errors are logged and counted by TriggerConfigReload.
				_ = t.TriggerConfigReload()
			})
			viper.WatchConfig()
		}

		return t.Run(cmd.Context())
	},
}

// loadServerConfig resolves the server configuration: flags, then ATC_* environment
// variables, then the config file.
func loadServerConfig(cmd *cobra.Command) (atc.Config, error) {
	if configFile := viper.GetString("config"); configFile != "" {
		viper.SetConfigFile(configFile)
		if err := viper.ReadInConfig(); err != nil {
			return atc.Config{}, fmt.Errorf("failed to read config file %s: %w", configFile, err)
		}
	}
	cfg, err := atc.LoadConfigFromViper(viper.GetViper())
	if err != nil {
		return cfg, err
	}
	applyChangedServerFlags(cmd, &cfg)
	return cfg, nil
}

func applyChangedServerFlags(cmd *cobra.Command, cfg *atc.Config) {
	if cmd.Flags().Changed("port") {
		cfg.Server.HTTPListenPort = viper.GetInt("port")
	}
	if cmd.Flags().Changed("metrics_port") {
		cfg.Server.MetricsListenPort = viper.GetInt("metrics_port")
	}
	if cmd.Flags().Changed("mcp-port") {
		cfg.Server.McpListenPort = viper.GetInt("mcp_port")
	}
	if cmd.Flags().Changed("target") {
		cfg.Target = viper.GetStringSlice("target")
	}
	if cmd.Flags().Changed("log_level") {
		cfg.Server.LogLevel = viper.GetString("log_level")
	}
	if cmd.Flags().Changed("ui-enabled") {
		cfg.Server.UiEnabled = viper.GetBool("ui_enabled")
	}
	if cmd.Flags().Changed("mcp-enabled") {
		cfg.Server.McpEnabled = viper.GetBool("mcp_enabled")
	}
	if cmd.Flags().Changed("dry-run") {
		cfg.DryRun = viper.GetBool("dry_run")
	}
	if cmd.Flags().Changed("write-rate-limit") {
		cfg.WriteRateLimit = viper.GetString("write_rate_limit")
	}
	if cmd.Flags().Changed("listen-address") {
		cfg.Server.ListenAddress = viper.GetString("listen_address")
	}
	if cmd.Flags().Changed("tls-cert-file") {
		cfg.Server.TLSCertFile = viper.GetString("tls_cert_file")
	}
	if cmd.Flags().Changed("tls-key-file") {
		cfg.Server.TLSKeyFile = viper.GetString("tls_key_file")
	}
	if cmd.Flags().Changed("allow-unauthenticated") {
		cfg.Auth.AllowUnauthenticated = viper.GetBool("allow_unauthenticated")
	}
	if cmd.Flags().Changed("node-name") {
		cfg.NodeName = viper.GetString("node_name")
	}
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

	serverCmd.PersistentFlags().Bool("ui-enabled", true, "Enable serving the embedded web UI dashboard.")
	_ = viper.BindPFlag("ui_enabled", serverCmd.PersistentFlags().Lookup("ui-enabled"))

	serverCmd.PersistentFlags().Bool("mcp-enabled", true, "Enable serving the Model Context Protocol (MCP) server.")
	_ = viper.BindPFlag("mcp_enabled", serverCmd.PersistentFlags().Lookup("mcp-enabled"))

	serverCmd.PersistentFlags().Int("mcp-port", 8092, "port to expose MCP server on.")
	_ = viper.BindPFlag("mcp_port", serverCmd.PersistentFlags().Lookup("mcp-port"))

	serverCmd.PersistentFlags().String("listen-address", "", "Interface for the API/UI and MCP listeners (empty = all interfaces).")
	_ = viper.BindPFlag("listen_address", serverCmd.PersistentFlags().Lookup("listen-address"))

	serverCmd.PersistentFlags().String("tls-cert-file", "", "Serve the API/UI and MCP over HTTPS with this certificate (requires --tls-key-file).")
	_ = viper.BindPFlag("tls_cert_file", serverCmd.PersistentFlags().Lookup("tls-cert-file"))

	serverCmd.PersistentFlags().String("tls-key-file", "", "Private key for --tls-cert-file.")
	_ = viper.BindPFlag("tls_key_file", serverCmd.PersistentFlags().Lookup("tls-key-file"))

	serverCmd.PersistentFlags().Bool("allow-unauthenticated", false, "Allow running with auth disabled on a non-loopback listen address.")
	_ = viper.BindPFlag("allow_unauthenticated", serverCmd.PersistentFlags().Lookup("allow-unauthenticated"))

	serverCmd.PersistentFlags().Bool("dry-run", false, "Disable writing to Consul, log actions instead.")
	_ = viper.BindPFlag("dry_run", serverCmd.PersistentFlags().Lookup("dry-run"))

	serverCmd.PersistentFlags().String("write-rate-limit", "1s", "Coalesce write events within this duration window (e.g. 1s, 500ms).")
	_ = viper.BindPFlag("write_rate_limit", serverCmd.PersistentFlags().Lookup("write-rate-limit"))
}
