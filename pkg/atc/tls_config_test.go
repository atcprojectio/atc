package atc

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	atc_server "github.com/atcprojectio/atc/pkg/atc/server"
)

func TestValidateTLSSettings(t *testing.T) {
	base := Config{Target: []string{"all"}, Auth: AuthConfig{AllowUnauthenticated: true}}

	c := base
	c.Server = atc_server.Config{TLSCertFile: "/x/tls.crt"}
	assert.Len(t, ValidateConfig(c), 1, "cert without key")

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")
	require.NoError(t, os.WriteFile(bad, []byte("not a certificate"), 0o600))
	c.Server = atc_server.Config{TLSCertFile: bad, TLSKeyFile: bad}
	errs := ValidateConfig(c)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), "invalid TLS certificate or key")

	assert.Empty(t, ValidateConfig(base), "TLS is optional")
}

func TestTLSSettingsFromEnvironment(t *testing.T) {
	t.Setenv("ATC_TLS_CERT_FILE", "/etc/atc/tls.crt")
	t.Setenv("ATC_TLS_KEY_FILE", "/etc/atc/tls.key")
	v := viper.New()
	v.SetEnvPrefix("atc")
	v.AutomaticEnv()
	cfg, err := LoadConfigFromViper(v)
	require.NoError(t, err)
	assert.Equal(t, "/etc/atc/tls.crt", cfg.Server.TLSCertFile)
	assert.Equal(t, "/etc/atc/tls.key", cfg.Server.TLSKeyFile)
}

func TestSecurityHeaders(t *testing.T) {
	a := &Atc{Cfg: Config{Auth: AuthConfig{Enabled: true}}}
	h := a.requestGuard(false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://atc.internal/", nil))

	csp := rr.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'self'")
	assert.Contains(t, csp, "script-src 'self'")
	assert.Contains(t, csp, "connect-src 'self'", "an injected script must not be able to send the token elsewhere")
	assert.Contains(t, csp, "frame-ancestors 'none'")
	assert.NotContains(t, csp, "unsafe-inline")
	assert.NotContains(t, csp, "fonts.googleapis.com", "the dashboard must not depend on external hosts")
	assert.Equal(t, "no-referrer", rr.Header().Get("Referrer-Policy"))
}
