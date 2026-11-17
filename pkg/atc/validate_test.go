package atc

import (
	atc_server "github.com/atcprojectio/atc/pkg/atc/server"
	"testing"

	"github.com/atcprojectio/atc/pkg/atc/forwarder"
	"github.com/atcprojectio/atc/pkg/atc/redirector"
)

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name      string
		cfg       Config
		wantErrs  int
		errSubstr string
	}{
		{
			name: "valid default config",
			cfg: Config{
				Target:             []string{"all"},
				Server:             atc_server.Config{ListenAddress: "127.0.0.1"},
				DampeningPeriod:    "10s",
				MinDampeningPeriod: "5s",
				HA: HaConfig{
					Enabled:    true,
					SessionTTL: "15s",
				},
				Strategies: StrategiesConfig{
					Failover: map[string]forwarder.FailoverStrategy{
						"standard": {
							ConnectTimeout: "10s",
							Targets: []forwarder.FailoverTarget{
								{Datacenter: "dc2"},
							},
						},
					},
					Redirect: map[string]redirector.RedirectStrategy{
						"standard": {
							Datacenter: "dc2",
						},
					},
				},
			},
			wantErrs: 0,
		},
		{
			name: "invalid target module",
			cfg: Config{
				Target: []string{"invalid-module"},
			},
			wantErrs:  1,
			errSubstr: `invalid target module "invalid-module"`,
		},
		{
			name: "invalid dampening_period format",
			cfg: Config{
				DampeningPeriod: "invalid",
			},
			wantErrs:  1,
			errSubstr: `invalid dampening_period "invalid"`,
		},
		{
			name: "invalid min_dampening_period format",
			cfg: Config{
				MinDampeningPeriod: "invalid",
			},
			wantErrs:  1,
			errSubstr: `invalid min_dampening_period "invalid"`,
		},
		{
			name: "dampening less than min dampening",
			cfg: Config{
				DampeningPeriod:    "5s",
				MinDampeningPeriod: "10s",
			},
			wantErrs:  1,
			errSubstr: `dampening_period "5s" cannot be less than min_dampening_period "10s"`,
		},
		{
			name: "invalid HA session_ttl format",
			cfg: Config{
				HA: HaConfig{
					Enabled:    true,
					SessionTTL: "invalid",
				},
			},
			wantErrs:  1,
			errSubstr: `invalid HA session_ttl "invalid"`,
		},
		{
			name: "HA session_ttl too short",
			cfg: Config{
				HA: HaConfig{
					Enabled:    true,
					SessionTTL: "5s",
				},
			},
			wantErrs:  1,
			errSubstr: `HA session_ttl "5s" must be between 10s and 24h`,
		},
		{
			name: "HA session_ttl too long",
			cfg: Config{
				HA: HaConfig{
					Enabled:    true,
					SessionTTL: "25h",
				},
			},
			wantErrs:  1,
			errSubstr: `HA session_ttl "25h" must be between 10s and 24h`,
		},
		{
			name: "HA disabled session_ttl out of bounds ignored",
			cfg: Config{
				HA: HaConfig{
					Enabled:    false,
					SessionTTL: "5s",
				},
			},
			wantErrs: 0,
		},
		{
			name: "invalid failover connect_timeout",
			cfg: Config{
				Strategies: StrategiesConfig{
					Failover: map[string]forwarder.FailoverStrategy{
						"bad": {
							ConnectTimeout: "invalid",
						},
					},
				},
			},
			wantErrs:  1,
			errSubstr: `failover strategy "bad": invalid connect_timeout "invalid"`,
		},
		{
			name: "empty redirect strategy",
			cfg: Config{
				Strategies: StrategiesConfig{
					Redirect: map[string]redirector.RedirectStrategy{
						"bad": {},
					},
				},
			},
			wantErrs:  1,
			errSubstr: `redirect strategy "bad": must specify at least one target selector field`,
		},
		{
			name: "auth enabled without mechanisms",
			cfg: Config{
				Auth: AuthConfig{
					Enabled:               true,
					StaticKeys:            nil,
					ConsulTokenDelegation: false,
				},
			},
			wantErrs:  1,
			errSubstr: `auth is enabled, but neither static_keys nor consul_token_delegation is configured`,
		},
		{
			name: "auth enabled with insecure placeholder token",
			cfg: Config{
				Auth: AuthConfig{
					Enabled:    true,
					StaticKeys: []string{"atc-super-secret-token"},
				},
			},
			wantErrs:  1,
			errSubstr: `auth.static_keys contains insecure placeholder token "atc-super-secret-token"`,
		},
		{
			name:      "auth disabled on all interfaces is rejected",
			cfg:       Config{Target: []string{"all"}},
			wantErrs:  1,
			errSubstr: `exposing write endpoints without authentication`,
		},
		{
			name: "auth disabled on non-loopback address is rejected",
			cfg: Config{
				Target: []string{"all"},
				Server: atc_server.Config{ListenAddress: "10.0.0.5"},
			},
			wantErrs:  1,
			errSubstr: `exposing write endpoints without authentication`,
		},
		{
			name: "auth disabled on loopback is allowed",
			cfg: Config{
				Target: []string{"all"},
				Server: atc_server.Config{ListenAddress: "::1"},
			},
			wantErrs: 0,
		},
		{
			name: "auth disabled with explicit opt-in is allowed",
			cfg: Config{
				Target: []string{"all"},
				Auth:   AuthConfig{AllowUnauthenticated: true},
			},
			wantErrs: 0,
		},
		{
			name: "auth enabled on all interfaces is allowed",
			cfg: Config{
				Target: []string{"all"},
				Auth:   AuthConfig{Enabled: true, StaticKeys: []string{"a-real-key"}},
			},
			wantErrs: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateConfig(tt.cfg)
			if len(errs) != tt.wantErrs {
				t.Fatalf("expected %d errors, got %d: %v", tt.wantErrs, len(errs), errs)
			}
			if tt.wantErrs > 0 && tt.errSubstr != "" {
				matched := false
				for _, err := range errs {
					if stringContains(err.Error(), tt.errSubstr) {
						matched = true
						break
					}
				}
				if !matched {
					t.Errorf("expected error containing %q, got: %v", tt.errSubstr, errs)
				}
			}
		})
	}
}

func stringContains(s, substr string) bool {
	lenSub := len(substr)
	if lenSub == 0 {
		return true
	}
	lenS := len(s)
	for i := 0; i <= lenS-lenSub; i++ {
		if s[i:i+lenSub] == substr {
			return true
		}
	}
	return false
}

func TestValidateWriteRateLimit(t *testing.T) {
	base := Config{Target: []string{"all"}, Auth: AuthConfig{AllowUnauthenticated: true}}

	for _, ok := range []string{"", "1s", "500ms", "0s"} {
		c := base
		c.WriteRateLimit = ok
		if errs := ValidateConfig(c); len(errs) != 0 {
			t.Errorf("write_rate_limit %q should be valid, got %v", ok, errs)
		}
	}
	for _, bad := range []string{"10/s", "fast", "-1s"} {
		c := base
		c.WriteRateLimit = bad
		if errs := ValidateConfig(c); len(errs) != 1 {
			t.Errorf("write_rate_limit %q should be rejected, got %v", bad, errs)
		}
	}
}
