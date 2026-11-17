package atc

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const precedenceYAML = `
consul_addr: "yaml-consul:8500"
dry_run: true
write_rate_limit: "3s"
server:
  http_listen_port: 18088
  log_level: "debug"
  ui_enabled: false
  mcp_enabled: false
`

// newCLIViper mirrors cmd/root.go + cmd/server.go: env prefix, AutomaticEnv and flags bound
// with their CLI defaults, then the YAML file read in.
func newCLIViper(t *testing.T, args ...string) *viper.Viper {
	t.Helper()
	v := viper.New()
	v.SetEnvPrefix("atc")
	v.AutomaticEnv()
	v.SetDefault("log_level", "info")

	fs := pflag.NewFlagSet("server", pflag.ContinueOnError)
	fs.Int("port", 8088, "")
	fs.String("log_level", "info", "")
	fs.String("consul_addr", "", "")
	fs.Bool("dry-run", false, "")
	fs.Bool("ui-enabled", true, "")
	fs.Bool("mcp-enabled", true, "")
	fs.String("write-rate-limit", "1s", "")
	require.NoError(t, fs.Parse(args))
	for key, flag := range map[string]string{
		"port": "port", "log_level": "log_level", "consul_addr": "consul_addr", "dry_run": "dry-run",
		"ui_enabled": "ui-enabled", "mcp_enabled": "mcp-enabled", "write_rate_limit": "write-rate-limit",
	} {
		require.NoError(t, v.BindPFlag(key, fs.Lookup(flag)))
	}

	v.SetConfigType("yaml")
	require.NoError(t, v.ReadConfig(strings.NewReader(precedenceYAML)))
	return v
}

func TestConfigPrecedence_YAMLBeatsFlagDefaults(t *testing.T) {
	cfg, err := LoadConfigFromViper(newCLIViper(t))
	require.NoError(t, err)

	assert.Equal(t, 18088, cfg.Server.HTTPListenPort)
	assert.Equal(t, "debug", cfg.Server.LogLevel)
	assert.Equal(t, "yaml-consul:8500", cfg.ConsulAddr)
	assert.True(t, cfg.DryRun, "dry_run: true in YAML must not be reset by the flag default")
	assert.Equal(t, "3s", cfg.WriteRateLimit)
	assert.False(t, cfg.Server.UiEnabled, "ui_enabled: false in YAML must not be reset by the flag default")
	assert.False(t, cfg.Server.McpEnabled, "mcp_enabled: false in YAML must not be reset by the flag default")
}

func TestConfigPrecedence_EnvBeatsYAML(t *testing.T) {
	t.Setenv("ATC_PORT", "18200")
	t.Setenv("ATC_LOG_LEVEL", "warn")
	t.Setenv("ATC_CONSUL_ADDR", "env-consul:8500")
	t.Setenv("ATC_DRY_RUN", "false")
	t.Setenv("ATC_UI_ENABLED", "true")

	cfg, err := LoadConfigFromViper(newCLIViper(t))
	require.NoError(t, err)

	assert.Equal(t, 18200, cfg.Server.HTTPListenPort)
	assert.Equal(t, "warn", cfg.Server.LogLevel)
	assert.Equal(t, "env-consul:8500", cfg.ConsulAddr)
	assert.False(t, cfg.DryRun)
	assert.True(t, cfg.Server.UiEnabled)
}

func TestConfigPrecedence_FlagsBeatYAML(t *testing.T) {
	cfg, err := LoadConfigFromViper(newCLIViper(t, "--consul_addr=flag-consul:8500"))
	require.NoError(t, err)
	assert.Equal(t, "flag-consul:8500", cfg.ConsulAddr)
}
