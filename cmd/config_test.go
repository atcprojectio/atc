package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setFlag sets a root persistent flag for one test and restores it afterwards.
func setFlag(t *testing.T, name, value string) {
	t.Helper()
	f := rootCmd.PersistentFlags().Lookup(name)
	require.NotNil(t, f, name)
	old, oldChanged := f.Value.String(), f.Changed
	require.NoError(t, rootCmd.PersistentFlags().Set(name, value))
	t.Cleanup(func() {
		_ = f.Value.Set(old)
		f.Changed = oldChanged
	})
}

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "atc.yaml")
	require.NoError(t, os.WriteFile(p, []byte(yaml), 0o600))
	return p
}

func TestLoadCLIConfigPrecedence(t *testing.T) {
	setFlag(t, "config", writeConfig(t, "consul_addr: \"yaml:8500\"\nconsul_dc: \"yaml-dc\"\nnode_name: \"yaml-node\"\n"))

	cfg, err := loadCLIConfig()
	require.NoError(t, err)
	assert.Equal(t, "yaml:8500", cfg.ConsulAddr)
	assert.Equal(t, "yaml-node", cfg.NodeName)

	// ATC_* environment variables override the file (they were ignored by "atc leader").
	t.Setenv("ATC_CONSUL_ADDR", "env:8500")
	t.Setenv("ATC_NODE_NAME", "env-node")
	cfg, err = loadCLIConfig()
	require.NoError(t, err)
	assert.Equal(t, "env:8500", cfg.ConsulAddr)
	assert.Equal(t, "env-node", cfg.NodeName)
	assert.Equal(t, "yaml-dc", cfg.ConsulDC)

	// Flags override both.
	setFlag(t, "consul_addr", "flag:8500")
	cfg, err = loadCLIConfig()
	require.NoError(t, err)
	assert.Equal(t, "flag:8500", cfg.ConsulAddr)
}

func TestLeaderStatusAcceptsConsulFlags(t *testing.T) {
	var kvReads atomic.Int32
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kvReads.Add(1)
		w.WriteHeader(http.StatusNotFound) // no lock held
	}))
	defer consul.Close()

	setFlag(t, "config", "")
	rootCmd.SetArgs([]string{"leader", "status", "--consul_addr", consul.Listener.Addr().String(), "--node-name", "cli-test"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	require.NoError(t, rootCmd.Execute(), "connection flags must be accepted by admin commands")
	assert.Positive(t, kvReads.Load(), "leader status must query the Consul given by --consul_addr")
}
