package atc

import (
	"fmt"

	"github.com/spf13/viper"
)

// LoadConfigFromViper unmarshals configuration from a Viper instance and applies flag/env defaults.
func LoadConfigFromViper(v *viper.Viper) (Config, error) {
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return cfg, fmt.Errorf("failed to unmarshal configuration: %w", err)
	}

	ApplyViperDefaults(v, &cfg)
	return cfg, nil
}

// ApplyViperDefaults populates configuration fields with default values from Viper flags/env.
func ApplyViperDefaults(v *viper.Viper, cfg *Config) {
	if cfg.Server.HTTPListenPort == 0 {
		cfg.Server.HTTPListenPort = v.GetInt("port")
	}
	if cfg.Server.MetricsListenPort == 0 {
		cfg.Server.MetricsListenPort = v.GetInt("metrics_port")
	}
	if cfg.Server.McpListenPort == 0 {
		cfg.Server.McpListenPort = v.GetInt("mcp_port")
	}
	if len(cfg.Target) == 0 {
		cfg.Target = v.GetStringSlice("target")
	}
	if cfg.Server.LogLevel == "" {
		cfg.Server.LogLevel = v.GetString("log_level")
	}
	if cfg.ConsulAddr == "" {
		cfg.ConsulAddr = v.GetString("consul_addr")
	}
	if cfg.ConsulToken == "" {
		cfg.ConsulToken = v.GetString("consul_token")
	}
	if cfg.ConsulDC == "" {
		cfg.ConsulDC = v.GetString("consul_dc")
	}
	if cfg.ConsulNamespace == "" {
		cfg.ConsulNamespace = v.GetString("consul_namespace")
	}
	if cfg.WriteRateLimit == "" {
		cfg.WriteRateLimit = v.GetString("write_rate_limit")
	}
	if !v.IsSet("server.ui_enabled") {
		cfg.Server.UiEnabled = v.GetBool("ui_enabled")
	}
	if !v.IsSet("server.mcp_enabled") {
		cfg.Server.McpEnabled = v.GetBool("mcp_enabled")
	}
	cfg.Server.MetricsNamespace = "atc"
	if !cfg.DryRun {
		cfg.DryRun = v.GetBool("dry_run")
	}
}
