package forwarder

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
	tracer = otel.Tracer("atc/forwarder")
	meter  = otel.Meter("atc/forwarder")

	reconcileCounter, _ = meter.Int64Counter(
		"atc_forwarder_reconcile_runs_total",
		metric.WithDescription("Total number of forwarder reconciliation runs"),
	)
	reconcileDuration, _ = meter.Float64Histogram(
		"atc_forwarder_reconcile_duration_seconds",
		metric.WithDescription("Duration of forwarder reconciliation runs in seconds"),
	)
)

type FailoverTarget struct {
	Service       string `yaml:"service" json:"service" mapstructure:"service"`
	Datacenter    string `yaml:"datacenter" json:"datacenter" mapstructure:"datacenter"`
	Namespace     string `yaml:"namespace" json:"namespace" mapstructure:"namespace"`
	ServiceSubset string `yaml:"service_subset" json:"service_subset" mapstructure:"service_subset"`
}

type FailoverStrategy struct {
	ConnectTimeout string           `yaml:"connect_timeout" json:"connect_timeout" mapstructure:"connect_timeout"`
	Targets        []FailoverTarget `yaml:"targets" json:"targets" mapstructure:"targets"`
}

type cachedStrategies struct {
	failover string
	redirect string
}

type Forwarder struct {
	logger             *slog.Logger
	consulAddr         string
	consulToken        string
	consulDC           string
	watcher            *watcher.ConsulWatcher
	failoverStrategies map[string]FailoverStrategy

	mu              sync.RWMutex
	strategiesCache map[string]cachedStrategies
}

func New(logger *slog.Logger, consulAddr, consulToken, consulDC string, failoverStrategies map[string]FailoverStrategy) (*Forwarder, error) {
	return &Forwarder{
		logger:             logger,
		consulAddr:         consulAddr,
		consulToken:        consulToken,
		consulDC:           consulDC,
		watcher:            watcher.New(logger, consulAddr, consulToken, consulDC),
		failoverStrategies: failoverStrategies,
		strategiesCache:    make(map[string]cachedStrategies),
	}, nil
}

func (f *Forwarder) GetCachedStrategies(svcName string) (string, string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if c, ok := f.strategiesCache[svcName]; ok {
		return c.failover, c.redirect
	}
	return "", ""
}

func (f *Forwarder) Run(ctx context.Context) error {
	client, err := api.NewClient(consulCfg(f.consulAddr, f.consulToken, f.consulDC))
	if err != nil {
		return fmt.Errorf("failed to create consul client: %w", err)
	}

	events := f.watcher.Events.Subscribe()
	defer f.watcher.Events.Unsubscribe(events)

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return f.watcher.Run(ctx)
	})

	g.Go(func() error {
		f.logger.InfoContext(ctx, "Starting initial forwarder catalog reconciliation")
		if err := f.reconcile(ctx, client); err != nil {
			f.logger.ErrorContext(ctx, "Initial reconciliation failed", slog.Any("error", err))
		}

		for {
			select {
			case <-ctx.Done():
				return nil
			case msg, ok := <-events:
				if !ok {
					return nil
				}
				f.logger.InfoContext(ctx, "Received watcher event, running reconciliation", slog.String("event", msg))
				if err := f.reconcile(ctx, client); err != nil {
					f.logger.ErrorContext(ctx, "Reconciliation failed", slog.Any("error", err))
				}
			}
		}
	})

	return g.Wait()
}

func (f *Forwarder) reconcile(ctx context.Context, client *api.Client) (err error) {
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

	localDC := getLocalDatacenter(client, f.consulDC)
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
	f.mu.Lock()
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			f.strategiesCache[svcName] = cachedStrategies{
				failover: getStrategyFromTags(tags, "atc.failover="),
				redirect: getStrategyFromTags(tags, "atc.redirect="),
			}
		}
	}
	f.mu.Unlock()

	// 2. Reconcile active catalog services
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			// Determine failover strategy name
			failoverStrategyName := getStrategyFromTags(tags, "atc.failover=")

			var targets []api.ServiceResolverFailoverTarget
			connectTimeout := 15 * time.Second

			if failoverStrategyName != "" {
				if strategy, ok := f.failoverStrategies[failoverStrategyName]; ok {
					if strategy.ConnectTimeout != "" {
						if d, err := time.ParseDuration(strategy.ConnectTimeout); err == nil {
							connectTimeout = d
						}
					}
					for _, target := range strategy.Targets {
						tSvc := target.Service
						if tSvc == "" {
							tSvc = svcName
						}
						tDC := target.Datacenter
						if tDC == "" {
							tDC = targetDC
						}
						targets = append(targets, api.ServiceResolverFailoverTarget{
							Service:       tSvc,
							Datacenter:    tDC,
							Namespace:     target.Namespace,
							ServiceSubset: target.ServiceSubset,
						})
					}
				}
			}

			if len(targets) == 0 {
				targets = []api.ServiceResolverFailoverTarget{
					{
						Service:    svcName,
						Datacenter: targetDC,
					},
				}
			}

			existing, exists := existingResolverEntries[svcName]
			needsCreateOrUpdate := !exists ||
				existing.Meta == nil ||
				existing.Meta["created-by"] != "atc" ||
				existing.Failover == nil ||
				len(existing.Failover) == 0 ||
				existing.Meta["failover-strategy"] != failoverStrategyName ||
				existing.Meta["redirect-strategy"] != "" ||
				existing.ConnectTimeout != connectTimeout

			if !needsCreateOrUpdate && existing.Failover != nil {
				fo, ok := existing.Failover["*"]
				if !ok || len(fo.Targets) != len(targets) {
					needsCreateOrUpdate = true
				} else {
					for i, t := range fo.Targets {
						if t.Service != targets[i].Service ||
							t.Datacenter != targets[i].Datacenter ||
							t.Namespace != targets[i].Namespace ||
							t.ServiceSubset != targets[i].ServiceSubset {
							needsCreateOrUpdate = true
							break
						}
					}
				}
			}

			if needsCreateOrUpdate {
				f.logger.InfoContext(ctx, "Creating/Updating failover service-resolver for active service", slog.String("service", svcName), slog.String("failover-strategy", failoverStrategyName))
				entry := &api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: svcName,
					Meta: map[string]string{
						"created-by":        "atc",
						"failover-strategy": failoverStrategyName,
					},
					ConnectTimeout: connectTimeout,
					Failover: map[string]api.ServiceResolverFailover{
						"*": {
							Targets: targets,
						},
					},
				}
				_, _, err = client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
				if err != nil {
					f.logger.ErrorContext(ctx, "Failed to set failover service-resolver", slog.String("service", svcName), slog.Any("error", err))
				}
			}
		} else {
			// Service exists locally but does not have the tag.
			// If we created a config entry, we must delete it.
			if existing, exists := existingResolverEntries[svcName]; exists {
				if existing.Meta != nil && existing.Meta["created-by"] == "atc" {
					f.logger.InfoContext(ctx, "Deleting service-resolver because atc.enabled tag was removed", slog.String("service", svcName))
					_, err = client.ConfigEntries().Delete("service-resolver", svcName, (&api.WriteOptions{}).WithContext(ctx))
					if err != nil {
						f.logger.ErrorContext(ctx, "Failed to delete service-resolver", slog.String("service", svcName), slog.Any("error", err))
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
