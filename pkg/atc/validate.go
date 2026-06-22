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

	// Validate HA Leader Election
	if cfg.HA.Enabled {
		if cfg.HA.SessionTTL != "" {
			d, err := time.ParseDuration(cfg.HA.SessionTTL)
			if err != nil {
				errs = append(errs, fmt.Errorf("invalid HA session_ttl %q: %w", cfg.HA.SessionTTL, err))
			} else {
				// Consul session TTL must be between 10s and 24h
				if d < 10*time.Second || d > 24*time.Hour {
					errs = append(errs, fmt.Errorf("HA session_ttl %q must be between 10s and 24h", cfg.HA.SessionTTL))
				}
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

	return errs
}
