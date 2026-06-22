package atc

import (
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
