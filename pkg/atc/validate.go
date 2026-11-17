package atc

import (
	"fmt"
	"slices"
	"time"
)

// ValidTargets holds the list of acceptable target modules.
var ValidTargets = []string{"all", "consul", "forwarder", "redirector", "server"}

// ValidateConfig performs semantic and syntactical validation on the ATC config.
func ValidateConfig(cfg Config) []error {
	var errs []error

	// Validate Target modules
	for _, target := range cfg.Target {
		if !slices.Contains(ValidTargets, target) {
			errs = append(errs, fmt.Errorf("invalid target module %q: must be one of %v", target, ValidTargets))
		}
	}

	// Validate Dampening periods
	if cfg.DampeningPeriod != "" {
		if _, err := time.ParseDuration(cfg.DampeningPeriod); err != nil {
			errs = append(errs, fmt.Errorf("invalid dampening_period %q: %w", cfg.DampeningPeriod, err))
		}
	}

	if cfg.MinDampeningPeriod != "" {
		if _, err := time.ParseDuration(cfg.MinDampeningPeriod); err != nil {
			errs = append(errs, fmt.Errorf("invalid min_dampening_period %q: %w", cfg.MinDampeningPeriod, err))
		}
	}

	if cfg.DampeningPeriod != "" && cfg.MinDampeningPeriod != "" {
		dp, err1 := time.ParseDuration(cfg.DampeningPeriod)
		mdp, err2 := time.ParseDuration(cfg.MinDampeningPeriod)
		if err1 == nil && err2 == nil && dp < mdp {
			errs = append(errs, fmt.Errorf("dampening_period %q cannot be less than min_dampening_period %q", cfg.DampeningPeriod, cfg.MinDampeningPeriod))
		}
	}

	if cfg.ResyncInterval != "" {
		if d, err := time.ParseDuration(cfg.ResyncInterval); err != nil {
			errs = append(errs, fmt.Errorf("invalid resync_interval %q: %w", cfg.ResyncInterval, err))
		} else if d < 0 {
			errs = append(errs, fmt.Errorf("resync_interval %q must not be negative", cfg.ResyncInterval))
		}
	}

	// Validate HA Leader Election
	if cfg.HA.Enabled {
		if cfg.HA.SessionTTL != "" {
			d, err := time.ParseDuration(cfg.HA.SessionTTL)
			if err != nil {
				errs = append(errs, fmt.Errorf("invalid HA session_ttl %q: %w", cfg.HA.SessionTTL, err))
			} else if d < 10*time.Second || d > 24*time.Hour {
				// Consul session TTL must be between 10s and 24h
				errs = append(errs, fmt.Errorf("HA session_ttl %q must be between 10s and 24h", cfg.HA.SessionTTL))
			}
		}
	}

	// Validate Failover Strategies
	for name, strat := range cfg.Strategies.Failover {
		if strat.ConnectTimeout != "" {
			if _, err := time.ParseDuration(strat.ConnectTimeout); err != nil {
				errs = append(errs, fmt.Errorf("failover strategy %q: invalid connect_timeout %q: %w", name, strat.ConnectTimeout, err))
			}
		}
	}

	// Validate Redirect Strategies
	for name, strat := range cfg.Strategies.Redirect {
		if strat.Service == "" && strat.Datacenter == "" && strat.Namespace == "" && strat.ServiceSubset == "" {
			errs = append(errs, fmt.Errorf("redirect strategy %q: must specify at least one target selector field (service, datacenter, namespace, or service_subset)", name))
		}
	}

	// Validate Authentication Configuration
	if cfg.Auth.Enabled {
		if len(cfg.Auth.StaticKeys) == 0 && !cfg.Auth.ConsulTokenDelegation {
			errs = append(errs, fmt.Errorf("auth is enabled, but neither static_keys nor consul_token_delegation is configured"))
		}
		if slices.Contains(cfg.Auth.StaticKeys, "atc-super-secret-token") {
			errs = append(errs, fmt.Errorf("auth.static_keys contains insecure placeholder token %q", "atc-super-secret-token"))
		}
	}

	// Refuse to expose unauthenticated write endpoints (overrides, purge, reload, MCP tools)
	// to the network unless the operator explicitly opts in.
	if resolveModules(cfg.Target)[Server] && !cfg.Auth.Enabled && !cfg.Auth.AllowUnauthenticated &&
		!IsLoopbackListenAddress(cfg.Server.ListenAddress) {
		errs = append(errs, fmt.Errorf("auth is disabled but the API/MCP listeners bind to %q, exposing write endpoints without authentication; "+
			"enable auth, set server.listen_address to a loopback address (e.g. 127.0.0.1), "+
			"or set auth.allow_unauthenticated: true (--allow-unauthenticated) to accept the risk", listenAddrForMsg(cfg.Server.ListenAddress)))
	}

	return errs
}

func listenAddrForMsg(addr string) string {
	if addr == "" {
		return "all interfaces"
	}
	return addr
}
