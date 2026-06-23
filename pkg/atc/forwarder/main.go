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
	"go.opentelemetry.io/otel/trace"
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

type writeCanceler interface {
	CancelPendingWrite(svcName string)
}

type Forwarder struct {
	logger             *slog.Logger
	consulAddr         string
	consulToken        string
	consulDC           string
	consulNamespace    string
	writeRateLimit     string
	watcher            *watcher.ConsulWatcher
	failoverStrategies map[string]FailoverStrategy
	dampeningPeriod    string
	minDampeningPeriod string
	dryRun             bool

	mu              sync.RWMutex
	strategiesCache map[string]cachedStrategies

	// Hysteresis scheduling state
	schedMu       sync.Mutex
	pendingWrites map[string]*api.ServiceResolverConfigEntry
	writeTimers   map[string]*time.Timer
	redirector    writeCanceler
}

func New(logger *slog.Logger, consulAddr, consulToken, consulDC, consulNamespace, writeRateLimit string, failoverStrategies map[string]FailoverStrategy, dampeningPeriod, minDampeningPeriod string, dryRun bool) (*Forwarder, error) {
	return &Forwarder{
		logger:             logger,
		consulAddr:         consulAddr,
		consulToken:        consulToken,
		consulDC:           consulDC,
		consulNamespace:    consulNamespace,
		writeRateLimit:     writeRateLimit,
		watcher:            watcher.New(logger, consulAddr, consulToken, consulDC, consulNamespace),
		failoverStrategies: failoverStrategies,
		dampeningPeriod:    dampeningPeriod,
		minDampeningPeriod: minDampeningPeriod,
		dryRun:             dryRun,
		strategiesCache:    make(map[string]cachedStrategies),
		pendingWrites:      make(map[string]*api.ServiceResolverConfigEntry),
		writeTimers:        make(map[string]*time.Timer),
	}, nil
}

func (f *Forwarder) SetRedirector(r writeCanceler) {
	f.schedMu.Lock()
	defer f.schedMu.Unlock()
	f.redirector = r
}

func (f *Forwarder) CancelPendingWrite(svcName string) {
	f.schedMu.Lock()
	defer f.schedMu.Unlock()
	if timer, ok := f.writeTimers[svcName]; ok {
		timer.Stop()
		delete(f.writeTimers, svcName)
	}
	delete(f.pendingWrites, svcName)
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
	client, err := api.NewClient(consulCfg(f.consulAddr, f.consulToken, f.consulDC, f.consulNamespace))
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

		var (
			debounceTimer *time.Timer
			timerChan     <-chan time.Time
		)

		defer func() {
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
		}()

		for {
			select {
			case <-ctx.Done():
				return nil
			case msg, ok := <-events:
				if !ok {
					return nil
				}
				f.logger.InfoContext(ctx, "Received watcher event", slog.String("event", msg))

				f.mu.RLock()
				rateLimitStr := f.writeRateLimit
				f.mu.RUnlock()

				var rateLimit time.Duration
				if rateLimitStr != "" {
					if d, err := time.ParseDuration(rateLimitStr); err == nil {
						rateLimit = d
					}
				}

				if rateLimit <= 0 {
					f.logger.DebugContext(ctx, "No write rate limit set, running reconciliation immediately")
					if err := f.reconcile(ctx, client); err != nil {
						f.logger.ErrorContext(ctx, "Reconciliation failed", slog.Any("error", err))
					}
				} else {
					if debounceTimer != nil {
						debounceTimer.Stop()
					}
					f.logger.InfoContext(ctx, "Coalescing/debouncing watcher events, scheduling reconciliation", slog.Duration("window", rateLimit))
					debounceTimer = time.NewTimer(rateLimit)
					timerChan = debounceTimer.C
				}

			case <-timerChan:
				timerChan = nil
				f.logger.InfoContext(ctx, "Coalescing window elapsed, running reconciliation")
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

	// Update cache and get config snapshot
	f.mu.Lock()
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			foStrat := getStrategyFromTags(tags, "atc.failover=")
			if foStrat == "" {
				if _, ok := f.failoverStrategies["default"]; ok {
					foStrat = "default"
				}
			}
			f.strategiesCache[svcName] = cachedStrategies{
				failover: foStrat,
				redirect: getStrategyFromTags(tags, "atc.redirect="),
			}
		}
	}
	failoverStrategies := f.failoverStrategies
	dampeningPeriod := f.dampeningPeriod
	minDampeningPeriod := f.minDampeningPeriod
	f.mu.Unlock()

	// 2. Reconcile active catalog services
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			if existing, exists := existingResolverEntries[svcName]; exists {
				if existing.Meta != nil && existing.Meta["created-by"] == "atc-override" {
					f.logger.DebugContext(ctx, "Skipping reconciliation because service-resolver has an active manual override", slog.String("service", svcName))
					continue
				}
			}

			// Cancel any pending redirect writes in redirector
			if f.redirector != nil {
				f.redirector.CancelPendingWrite(svcName)
			}

			// Determine failover strategy name
			failoverStrategyName := getStrategyFromTags(tags, "atc.failover=")
			if failoverStrategyName == "" {
				if _, ok := failoverStrategies["default"]; ok {
					failoverStrategyName = "default"
				}
			}

			var targets []api.ServiceResolverFailoverTarget
			connectTimeout := 15 * time.Second

			if failoverStrategyName != "" {
				if strategy, ok := failoverStrategies[failoverStrategyName]; ok {
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

				dampening := getDampeningDuration(tags, dampeningPeriod, minDampeningPeriod)
				if dampening <= 0 {
					f.CancelPendingWrite(svcName)
					if f.dryRun {
						span.AddEvent("dry_run_set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
						f.logger.InfoContext(ctx, "[DRY RUN] Would create/update failover service-resolver immediately", slog.String("service", svcName), slog.String("failover-strategy", failoverStrategyName))
					} else {
						span.AddEvent("set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
						f.logger.InfoContext(ctx, "Creating/Updating failover service-resolver immediately", slog.String("service", svcName), slog.String("failover-strategy", failoverStrategyName))
						_, _, err = client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
						if err != nil {
							f.logger.ErrorContext(ctx, "Failed to set failover service-resolver", slog.String("service", svcName), slog.Any("error", err))
						}
					}
				} else {
					f.logger.InfoContext(ctx, "Scheduling failover service-resolver write", slog.String("service", svcName), slog.Duration("dampening", dampening))
					f.scheduleWrite(svcName, entry, dampening)
				}
			}
		} else {
			// Service exists locally but does not have the tag.
			// If we created a config entry, we must delete it.
			if existing, exists := existingResolverEntries[svcName]; exists {
				if existing.Meta != nil && existing.Meta["created-by"] == "atc-override" {
					continue
				}
				if existing.Meta != nil && existing.Meta["created-by"] == "atc" {
					if f.dryRun {
						span.AddEvent("dry_run_delete_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
						f.logger.InfoContext(ctx, "[DRY RUN] Would delete service-resolver because atc.enabled tag was removed", slog.String("service", svcName))
					} else {
						span.AddEvent("delete_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
						f.logger.InfoContext(ctx, "Deleting service-resolver because atc.enabled tag was removed", slog.String("service", svcName))
						_, err = client.ConfigEntries().Delete("service-resolver", svcName, (&api.WriteOptions{}).WithContext(ctx))
						if err != nil {
							f.logger.ErrorContext(ctx, "Failed to delete service-resolver", slog.String("service", svcName), slog.Any("error", err))
						}
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

func consulCfg(addr, token, dc, ns string) *api.Config {
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
	if ns != "" {
		cfg.Namespace = ns
	}
	return cfg
}

func getDampeningDuration(tags []string, globalDefault, minLimit string) time.Duration {
	d := 0 * time.Second
	if globalDefault != "" {
		if val, err := time.ParseDuration(globalDefault); err == nil {
			d = val
		}
	}

	tagVal := getStrategyFromTags(tags, "atc.dampening=")
	if tagVal != "" {
		if val, err := time.ParseDuration(tagVal); err == nil {
			d = val
		}
	}

	if minLimit != "" {
		if minVal, err := time.ParseDuration(minLimit); err == nil {
			if d < minVal {
				d = minVal
			}
		}
	}
	return d
}

func (f *Forwarder) scheduleWrite(svcName string, entry *api.ServiceResolverConfigEntry, dampening time.Duration) {
	f.schedMu.Lock()
	defer f.schedMu.Unlock()

	if timer, ok := f.writeTimers[svcName]; ok {
		if !entriesEqual(f.pendingWrites[svcName], entry) {
			f.pendingWrites[svcName] = entry
			timer.Reset(dampening)
		}
	} else {
		f.pendingWrites[svcName] = entry
		f.writeTimers[svcName] = time.AfterFunc(dampening, func() {
			f.executeScheduledWrite(svcName)
		})
	}
}

func (f *Forwarder) executeScheduledWrite(svcName string) {
	f.schedMu.Lock()
	entry, ok := f.pendingWrites[svcName]
	delete(f.pendingWrites, svcName)
	delete(f.writeTimers, svcName)
	f.schedMu.Unlock()

	if !ok || entry == nil {
		return
	}

	ctx, span := tracer.Start(context.Background(), "executeScheduledWrite")
	defer span.End()

	client, err := api.NewClient(consulCfg(f.consulAddr, f.consulToken, f.consulDC, f.consulNamespace))
	if err != nil {
		f.logger.Error("Failed to create Consul client for scheduled write", slog.String("service", svcName), slog.Any("error", err))
		span.RecordError(err)
		return
	}

	if f.dryRun {
		span.AddEvent("dry_run_scheduled_set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
		f.logger.Info("[DRY RUN] Would execute scheduled failover write for service-resolver", slog.String("service", svcName))
	} else {
		span.AddEvent("scheduled_set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
		f.logger.Info("Executing scheduled failover write for service-resolver", slog.String("service", svcName))
		_, _, err = client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
		if err != nil {
			f.logger.Error("Failed to write scheduled failover service-resolver entry", slog.String("service", svcName), slog.Any("error", err))
			span.RecordError(err)
		}
	}
}

func entriesEqual(a, b *api.ServiceResolverConfigEntry) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Name != b.Name || a.ConnectTimeout != b.ConnectTimeout {
		return false
	}
	if len(a.Meta) != len(b.Meta) {
		return false
	}
	for k, v := range a.Meta {
		if b.Meta[k] != v {
			return false
		}
	}
	// Compare Failovers
	if len(a.Failover) != len(b.Failover) {
		return false
	}
	for k, v := range a.Failover {
		vb, ok := b.Failover[k]
		if !ok || len(v.Targets) != len(vb.Targets) {
			return false
		}
		for i, t := range v.Targets {
			tb := vb.Targets[i]
			if t.Service != tb.Service || t.Datacenter != tb.Datacenter || t.Namespace != tb.Namespace || t.ServiceSubset != tb.ServiceSubset {
				return false
			}
		}
	}
	// Compare Redirects
	if (a.Redirect == nil) != (b.Redirect == nil) {
		return false
	}
	if a.Redirect != nil && b.Redirect != nil {
		if a.Redirect.Service != b.Redirect.Service || a.Redirect.Datacenter != b.Redirect.Datacenter || a.Redirect.Namespace != b.Redirect.Namespace || a.Redirect.ServiceSubset != b.Redirect.ServiceSubset {
			return false
		}
	}
	return true
}

func (f *Forwarder) UpdateConfig(failoverStrategies map[string]FailoverStrategy, dampeningPeriod, minDampeningPeriod, consulNamespace, writeRateLimit string, dryRun bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failoverStrategies = failoverStrategies
	f.dampeningPeriod = dampeningPeriod
	f.minDampeningPeriod = minDampeningPeriod
	f.consulNamespace = consulNamespace
	f.writeRateLimit = writeRateLimit
	f.dryRun = dryRun
	f.logger.Info("Forwarder configuration reloaded dynamically",
		slog.Int("strategies_count", len(failoverStrategies)),
		slog.String("dampening_period", dampeningPeriod),
		slog.String("min_dampening_period", minDampeningPeriod),
		slog.String("consul_namespace", consulNamespace),
		slog.String("write_rate_limit", writeRateLimit),
		slog.Bool("dry_run", dryRun),
	)
}
