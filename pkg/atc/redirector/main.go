package redirector

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/atcprojectio/atc/pkg/atc/watcher"
	"github.com/hashicorp/consul/api"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/errgroup"
)

var (
	tracer = otel.Tracer("atc/redirector")
	meter  = otel.Meter("atc/redirector")

	reconcileCounter, _ = meter.Int64Counter(
		"atc_redirector_reconcile_runs_total",
		metric.WithDescription("Total number of redirector reconciliation runs"),
	)
	reconcileDuration, _ = meter.Float64Histogram(
		"atc_redirector_reconcile_duration_seconds",
		metric.WithDescription("Duration of redirector reconciliation runs in seconds"),
	)
)

type RedirectStrategy struct {
	Service       string `yaml:"service" json:"service" mapstructure:"service"`
	Datacenter    string `yaml:"datacenter" json:"datacenter" mapstructure:"datacenter"`
	Namespace     string `yaml:"namespace" json:"namespace" mapstructure:"namespace"`
	ServiceSubset string `yaml:"service_subset" json:"service_subset" mapstructure:"service_subset"`
}

type cachedStrategies struct {
	failover string
	redirect string
}

type Redirector struct {
	// ...
	logger             *slog.Logger
	consulAddr         string
	consulToken        string
	consulDC           string
	watcher            *watcher.ConsulWatcher
	forwarderEnabled   bool
	redirectStrategies map[string]RedirectStrategy

	mu              sync.RWMutex
	strategiesCache map[string]cachedStrategies
}

func New(logger *slog.Logger, consulAddr, consulToken, consulDC string, forwarderEnabled bool, redirectStrategies map[string]RedirectStrategy) (*Redirector, error) {
	return &Redirector{
		logger:             logger,
		consulAddr:         consulAddr,
		consulToken:        consulToken,
		consulDC:           consulDC,
		watcher:            watcher.New(logger, consulAddr, consulToken, consulDC),
		forwarderEnabled:   forwarderEnabled,
		redirectStrategies: redirectStrategies,
		strategiesCache:    make(map[string]cachedStrategies),
	}, nil
}

func (r *Redirector) GetCachedStrategies(svcName string) (string, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.strategiesCache[svcName]; ok {
		return c.failover, c.redirect
	}
	return "", ""
}

func (r *Redirector) Run(ctx context.Context) error {
	client, err := api.NewClient(consulCfg(r.consulAddr, r.consulToken, r.consulDC))
	if err != nil {
		return fmt.Errorf("failed to create consul client: %w", err)
	}

	events := r.watcher.Events.Subscribe()
	defer r.watcher.Events.Unsubscribe(events)

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return r.watcher.Run(ctx)
	})

	g.Go(func() error {
		r.logger.InfoContext(ctx, "Starting initial redirector catalog reconciliation")
		if err := r.reconcile(ctx, client); err != nil {
			r.logger.ErrorContext(ctx, "Initial reconciliation failed", slog.Any("error", err))
		}

		for {
			select {
			case <-ctx.Done():
				return nil
			case msg, ok := <-events:
				if !ok {
					return nil
				}
				r.logger.InfoContext(ctx, "Received watcher event, running reconciliation", slog.String("event", msg))
				if err := r.reconcile(ctx, client); err != nil {
					r.logger.ErrorContext(ctx, "Reconciliation failed", slog.Any("error", err))
				}
			}
		}
	})

	return g.Wait()
}

func (r *Redirector) reconcile(ctx context.Context, client *api.Client) (err error) {
	ctx, span := tracer.Start(ctx, "reconcile")
	defer span.End()

	startTime := time.Now()
	defer func() {
		duration := time.Since(startTime).Seconds()
		status := "success"
		if err != nil {
			status = "failure"
			span.RecordError(err)
		}
		reconcileCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("status", status),
		))
		reconcileDuration.Record(ctx, duration, metric.WithAttributes(
			attribute.String("status", status),
		))
	}()

	services, _, err := client.Catalog().Services((&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to fetch consul services: %w", err)
	}

	localDC := getLocalDatacenter(client, r.consulDC)
	targetDC, err := findTargetDatacenter(client, localDC)
	if err != nil {
		return err
	}

	// 1. Fetch all existing config entries of type service-resolver
	entries, _, err := client.ConfigEntries().List("service-resolver", (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to list service-resolver config entries: %w", err)
	}

	existingResolverEntries := make(map[string]*api.ServiceResolverConfigEntry)
	for _, entry := range entries {
		if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
			existingResolverEntries[res.Name] = res
		}
	}

	// Update cache
	r.mu.Lock()
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			r.strategiesCache[svcName] = cachedStrategies{
				failover: getStrategyFromTags(tags, "atc.failover="),
				redirect: getStrategyFromTags(tags, "atc.redirect="),
			}
		}
	}
	r.mu.Unlock()

	// 2. Reconcile active catalog services
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			// Determine failover strategy name
			failoverStrategyName := getStrategyFromTags(tags, "atc.failover=")

			existing, exists := existingResolverEntries[svcName]
			var needsSet bool
			if r.forwarderEnabled {
				needsSet = false
			} else {
				needsSet = !exists ||
					existing.Meta == nil ||
					existing.Meta["created-by"] != "atc" ||
					(existing.Redirect != nil && existing.Redirect.Service != "") ||
					existing.Meta["failover-strategy"] != failoverStrategyName ||
					existing.Meta["redirect-strategy"] != ""
			}

			if needsSet {
				r.logger.InfoContext(ctx, "Ensuring base/failover service-resolver for active service", slog.String("service", svcName))
				entry := &api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: svcName,
					Meta: map[string]string{
						"created-by":        "atc",
						"failover-strategy": failoverStrategyName,
					},
				}
				_, _, err = client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
				if err != nil {
					r.logger.ErrorContext(ctx, "Failed to set base service-resolver", slog.String("service", svcName), slog.Any("error", err))
				}
			}
		} else {
			// Service exists locally but does not have the tag.
			// If we created a config entry, we must delete it.
			if existing, exists := existingResolverEntries[svcName]; exists {
				if existing.Meta != nil && existing.Meta["created-by"] == "atc" {
					r.logger.InfoContext(ctx, "Deleting service-resolver because atc.enabled tag was removed", slog.String("service", svcName))
					_, err = client.ConfigEntries().Delete("service-resolver", svcName, (&api.WriteOptions{}).WithContext(ctx))
					if err != nil {
						r.logger.ErrorContext(ctx, "Failed to delete service-resolver", slog.String("service", svcName), slog.Any("error", err))
					}
				}
			}
		}
	}

	// 3. Reconcile deleted services (absent from catalog but config entry exists)
	for name, existing := range existingResolverEntries {
		if _, exists := services[name]; !exists {
			// Service is completely absent from the catalog.
			// If it was created by us, we must ensure it is a Redirect resolver entry.
			if existing.Meta != nil && existing.Meta["created-by"] == "atc" {
				// First check in-memory cache for redirect-strategy
				var redirectStrategyName string
				if _, cachedRedirect := r.GetCachedStrategies(name); cachedRedirect != "" {
					redirectStrategyName = cachedRedirect
				} else {
					// Fallback to existing metadata
					redirectStrategyName = existing.Meta["redirect-strategy"]
				}

				// Default values
				targetSvc := name
				targetDCVal := targetDC
				var targetNamespace string
				var targetServiceSubset string

				if redirectStrategyName != "" {
					if strategy, ok := r.redirectStrategies[redirectStrategyName]; ok {
						targetSvc = strategy.Service
						if targetSvc == "" {
							targetSvc = name
						}
						targetDCVal = strategy.Datacenter
						if targetDCVal == "" {
							targetDCVal = targetDC
						}
						targetNamespace = strategy.Namespace
						targetServiceSubset = strategy.ServiceSubset
					}
				}

				needsUpdate := existing.Redirect == nil ||
					existing.Redirect.Service != targetSvc ||
					existing.Redirect.Datacenter != targetDCVal ||
					existing.Redirect.Namespace != targetNamespace ||
					existing.Redirect.ServiceSubset != targetServiceSubset ||
					existing.Meta["failover-strategy"] != "" ||
					existing.Meta["redirect-strategy"] != redirectStrategyName

				if needsUpdate {
					r.logger.InfoContext(ctx, "Creating/Updating redirect service-resolver for deleted service", slog.String("service", name), slog.String("redirect-strategy", redirectStrategyName))
					entry := &api.ServiceResolverConfigEntry{
						Kind: "service-resolver",
						Name: name,
						Meta: map[string]string{
							"created-by":        "atc",
							"redirect-strategy": redirectStrategyName,
						},
						Redirect: &api.ServiceResolverRedirect{
							Service:       targetSvc,
							Datacenter:    targetDCVal,
							Namespace:     targetNamespace,
							ServiceSubset: targetServiceSubset,
						},
					}
					_, _, err = client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
					if err != nil {
						r.logger.ErrorContext(ctx, "Failed to set redirect service-resolver", slog.String("service", name), slog.Any("error", err))
					}
				}
			}
		}
	}

	return nil
}

func getLocalDatacenter(client *api.Client, configDC string) string {
	if configDC != "" {
		return configDC
	}
	self, err := client.Agent().Self()
	if err == nil {
		if config, ok := self["Config"]; ok {
			if dc, ok := config["Datacenter"].(string); ok {
				return dc
			}
		}
	}
	return "dc1"
}

func findTargetDatacenter(client *api.Client, localDC string) (string, error) {
	dcs, err := client.Catalog().Datacenters()
	if err != nil {
		return "", fmt.Errorf("failed to fetch datacenters: %w", err)
	}
	for _, dc := range dcs {
		if dc != localDC {
			return dc, nil
		}
	}
	return "dc2", nil
}

func getStrategyFromTags(tags []string, prefix string) string {
	for _, tag := range tags {
		if strings.HasPrefix(tag, prefix) {
			parts := strings.SplitN(tag, "=", 2)
			if len(parts) == 2 {
				return parts[1]
			}
		}
	}
	return ""
}

func consulCfg(addr, token, dc string) *api.Config {
	cfg := api.DefaultConfig()
	if addr != "" {
		cfg.Address = addr
	}
	if token != "" {
		cfg.Token = token
	}
	if dc != "" {
		cfg.Datacenter = dc
	}
	return cfg
}
