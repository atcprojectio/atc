package atc

import (
	"fmt"
	"os"

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

// ApplyViperDefaults populates configuration fields and ensures CLI flags and ATC_* env vars take precedence over YAML config values.
func ApplyViperDefaults(v *viper.Viper, cfg *Config) {
	if cfg.Server.HTTPListenPort == 0 || hasEnv("ATC_PORT") {
		if port := v.GetInt("port"); port != 0 {
			cfg.Server.HTTPListenPort = port
		}
	}
	if cfg.Server.MetricsListenPort == 0 || hasEnv("ATC_METRICS_PORT") {
		if port := v.GetInt("metrics_port"); port != 0 {
			cfg.Server.MetricsListenPort = port
		}
	}
	if cfg.Server.McpListenPort == 0 || hasEnv("ATC_MCP_PORT") {
		if port := v.GetInt("mcp_port"); port != 0 {
			cfg.Server.McpListenPort = port
		}
	}
	if len(cfg.Target) == 0 || hasEnv("ATC_TARGET") {
		if targets := v.GetStringSlice("target"); len(targets) > 0 {
			cfg.Target = targets
		}
	}
	if cfg.Server.LogLevel == "" || hasEnv("ATC_LOG_LEVEL") {
		if level := v.GetString("log_level"); level != "" {
			cfg.Server.LogLevel = level
		}
	}
	if val := v.GetString("node_name"); val != "" {
		cfg.NodeName = val
	}
	if val := v.GetString("consul_addr"); val != "" {
		cfg.ConsulAddr = val
	}
	if val := v.GetString("consul_token"); val != "" {
		cfg.ConsulToken = val
	}
	if val := v.GetString("consul_dc"); val != "" {
		cfg.ConsulDC = val
	}
	if val := v.GetString("consul_namespace"); val != "" {
		cfg.ConsulNamespace = val
	}
	if val := v.GetString("write_rate_limit"); val != "" && (cfg.WriteRateLimit == "" || hasEnv("ATC_WRITE_RATE_LIMIT") || v.InConfig("write_rate_limit")) {
		cfg.WriteRateLimit = val
	}
	if !v.IsSet("server.ui_enabled") || hasEnv("ATC_UI_ENABLED") {
		cfg.Server.UiEnabled = v.GetBool("ui_enabled")
	}
	if !v.IsSet("server.mcp_enabled") || hasEnv("ATC_MCP_ENABLED") {
		cfg.Server.McpEnabled = v.GetBool("mcp_enabled")
	}
	if !v.IsSet("server.listen_address") || hasEnv("ATC_LISTEN_ADDRESS") {
		if val := v.GetString("listen_address"); val != "" || hasEnv("ATC_LISTEN_ADDRESS") {
			cfg.Server.ListenAddress = val
		}
	}
	if v.GetBool("allow_unauthenticated") {
		cfg.Auth.AllowUnauthenticated = true
	} else if hasEnv("ATC_ALLOW_UNAUTHENTICATED") {
		cfg.Auth.AllowUnauthenticated = false
	}
	cfg.Server.MetricsNamespace = "atc"
	if v.GetBool("dry_run") || hasEnv("ATC_DRY_RUN") {
		cfg.DryRun = v.GetBool("dry_run")
	}
}

func hasEnv(key string) bool {
	_, ok := os.LookupEnv(key)
	return ok
}

