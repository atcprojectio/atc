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
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
)

var (
	tracer = otel.Tracer("atc/redirector")
)

type RedirectStrategy struct {
	Service       string `yaml:"service" json:"service" mapstructure:"service"`
	Datacenter    string `yaml:"datacenter" json:"datacenter" mapstructure:"datacenter"`
	Namespace     string `yaml:"namespace" json:"namespace" mapstructure:"namespace"`
	ServiceSubset string `yaml:"service_subset" json:"service_subset" mapstructure:"service_subset"`
}

type writeCanceler interface {
	CancelPendingWrite(ctx context.Context, svcName string)
}

type cachedStrategies struct {
	failover  string
	redirect  string
	dampening string
}

type Redirector struct {
	logger             *slog.Logger
	consulAddr         string
	consulToken        string
	consulDC           string
	consulNamespace    string
	writeRateLimit     string
	watcher            *watcher.ConsulWatcher
	forwarderEnabled   bool
	redirectStrategies map[string]RedirectStrategy
	dampeningPeriod    string
	minDampeningPeriod string
	hasDefaultFailover bool
	dryRun             bool

	mu              sync.RWMutex
	strategiesCache map[string]cachedStrategies

	// Hysteresis scheduling state
	schedMu       sync.Mutex
	pendingWrites map[string]*api.ServiceResolverConfigEntry
	writeTimers   map[string]*time.Timer
	forwarder     writeCanceler

	reconcileCounter        metric.Int64Counter
	reconcileDuration       metric.Float64Histogram
	consulRequestsCounter   metric.Int64Counter
	consulDurationHistogram metric.Float64Histogram
	coalescedEventsCounter  metric.Int64Counter
	dampeningTriggered      metric.Int64Counter
	dampeningCancelled      metric.Int64Counter
}

func New(logger *slog.Logger, consulAddr, consulToken, consulDC, consulNamespace, writeRateLimit string, forwarderEnabled bool, redirectStrategies map[string]RedirectStrategy, dampeningPeriod, minDampeningPeriod string, hasDefaultFailover bool, dryRun bool) (*Redirector, error) {
	meter := otel.Meter("atc/redirector")
	reconcileCounter, err := meter.Int64Counter(
		"atc_redirector_reconcile_runs_total",
		metric.WithDescription("Total number of redirector reconciliation runs"),
	)
	if err != nil {
		return nil, err
	}
	reconcileDuration, err := meter.Float64Histogram(
		"atc_redirector_reconcile_duration_seconds",
		metric.WithDescription("Duration of redirector reconciliation runs in seconds"),
	)
	if err != nil {
		return nil, err
	}

	consulRequestsCounter, err := meter.Int64Counter(
		"atc_consul_requests_total",
		metric.WithDescription("Total number of Consul API requests from redirector"),
	)
	if err != nil {
		return nil, err
	}

	consulDurationHistogram, err := meter.Float64Histogram(
		"atc_consul_request_duration_seconds",
		metric.WithDescription("Duration of Consul API requests from redirector in seconds"),
	)
	if err != nil {
		return nil, err
	}

	coalescedEventsCounter, err := meter.Int64Counter(
		"atc_reconcile_coalesced_events_total",
		metric.WithDescription("Total number of watch events coalesced (debounced)"),
	)
	if err != nil {
		return nil, err
	}

	dampeningTriggered, err := meter.Int64Counter(
		"atc_dampening_writes_triggered_total",
		metric.WithDescription("Total number of scheduled dampening writes"),
	)
	if err != nil {
		return nil, err
	}

	dampeningCancelled, err := meter.Int64Counter(
		"atc_dampening_writes_cancelled_total",
		metric.WithDescription("Total number of cancelled dampening writes"),
	)
	if err != nil {
		return nil, err
	}

	r := &Redirector{
		logger:                  logger,
		consulAddr:              consulAddr,
		consulToken:             consulToken,
		consulDC:                consulDC,
		consulNamespace:         consulNamespace,
		writeRateLimit:          writeRateLimit,
		watcher:                 watcher.New(logger, consulAddr, consulToken, consulDC, consulNamespace),
		forwarderEnabled:        forwarderEnabled,
		redirectStrategies:      redirectStrategies,
		dampeningPeriod:         dampeningPeriod,
		minDampeningPeriod:      minDampeningPeriod,
		hasDefaultFailover:      hasDefaultFailover,
		dryRun:                  dryRun,
		strategiesCache:         make(map[string]cachedStrategies),
		pendingWrites:           make(map[string]*api.ServiceResolverConfigEntry),
		writeTimers:             make(map[string]*time.Timer),
		reconcileCounter:        reconcileCounter,
		reconcileDuration:       reconcileDuration,
		consulRequestsCounter:   consulRequestsCounter,
		consulDurationHistogram: consulDurationHistogram,
		coalescedEventsCounter:  coalescedEventsCounter,
		dampeningTriggered:      dampeningTriggered,
		dampeningCancelled:      dampeningCancelled,
	}

	_, err = meter.Int64ObservableGauge(
		"atc_dampening_pending_writes",
		metric.WithDescription("Number of pending writes currently dampened"),
		metric.WithInt64Callback(func(_ context.Context, obsrv metric.Int64Observer) error {
			r.schedMu.Lock()
			val := int64(len(r.pendingWrites))
			r.schedMu.Unlock()
			obsrv.Observe(val, metric.WithAttributes(attribute.String("module", "redirector")))
			return nil
		}),
	)
	if err != nil {
		return nil, err
	}

	return r, nil
}

func (r *Redirector) SetForwarder(f writeCanceler) {
	r.schedMu.Lock()
	defer r.schedMu.Unlock()
	r.forwarder = f
}

func (r *Redirector) CancelPendingWrite(ctx context.Context, svcName string) {
	r.schedMu.Lock()
	defer r.schedMu.Unlock()
	if timer, ok := r.writeTimers[svcName]; ok {
		r.logger.Info("Cancelled pending service-resolver write", slog.String("service", svcName), slog.String("reason", "reconciliation_canceled"))
		
		span := trace.SpanFromContext(ctx)
		span.AddEvent("dampening_cancelled", trace.WithAttributes(
			attribute.String("service", svcName),
			attribute.String("reason", "reconciliation_canceled"),
		))

		if r.dampeningCancelled != nil {
			r.dampeningCancelled.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "cancelled")))
		}
		timer.Stop()
		delete(r.writeTimers, svcName)
	}
	delete(r.pendingWrites, svcName)
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
	client, err := api.NewClient(consulCfg(r.consulAddr, r.consulToken, r.consulDC, r.consulNamespace))
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
				r.logger.InfoContext(ctx, "Received watcher event", slog.String("event", msg))

				r.mu.RLock()
				rateLimitStr := r.writeRateLimit
				r.mu.RUnlock()

				var rateLimit time.Duration
				if rateLimitStr != "" {
					if d, err := time.ParseDuration(rateLimitStr); err == nil {
						rateLimit = d
					}
				}

				if rateLimit <= 0 {
					r.logger.DebugContext(ctx, "No write rate limit set, running reconciliation immediately")
					if err := r.reconcile(ctx, client); err != nil {
						r.logger.ErrorContext(ctx, "Reconciliation failed", slog.Any("error", err))
					}
				} else {
					if debounceTimer != nil {
						debounceTimer.Stop()
					}
					r.logger.InfoContext(ctx, "Coalescing/debouncing watcher events, scheduling reconciliation", slog.Duration("window", rateLimit))
					if r.coalescedEventsCounter != nil {
						r.coalescedEventsCounter.Add(ctx, 1)
					}
					debounceTimer = time.NewTimer(rateLimit)
					timerChan = debounceTimer.C
				}

			case <-timerChan:
				timerChan = nil
				r.logger.InfoContext(ctx, "Coalescing window elapsed, running reconciliation")
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
		r.reconcileCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("status", status),
		))
		r.reconcileDuration.Record(ctx, duration, metric.WithAttributes(
			attribute.String("status", status),
		))
	}()

	var services map[string][]string
	err = r.traceConsulCall(ctx, "list_services", func() error {
		var err error
		services, _, err = client.Catalog().Services((&api.QueryOptions{}).WithContext(ctx))
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to fetch consul services: %w", err)
	}

	localDC := getLocalDatacenter(client, r.consulDC)
	targetDC, err := findTargetDatacenter(client, localDC)
	if err != nil {
		return err
	}

	// 1. Fetch all existing config entries of type service-resolver
	var entries []api.ConfigEntry
	err = r.traceConsulCall(ctx, "list_config_entries", func() error {
		var err error
		entries, _, err = client.ConfigEntries().List("service-resolver", (&api.QueryOptions{}).WithContext(ctx))
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to list service-resolver config entries: %w", err)
	}

	existingResolverEntries := make(map[string]*api.ServiceResolverConfigEntry)
	for _, entry := range entries {
		if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
			existingResolverEntries[res.Name] = res
		}
	}

	r.mu.Lock()
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			foStrat := getStrategyFromTags(tags, "atc.failover=")
			if foStrat == "" && r.hasDefaultFailover {
				foStrat = "default"
			}
			rdStrat := getStrategyFromTags(tags, "atc.redirect=")
			if rdStrat == "" {
				if _, ok := r.redirectStrategies["default"]; ok {
					rdStrat = "default"
				}
			}
			r.strategiesCache[svcName] = cachedStrategies{
				failover:  foStrat,
				redirect:  rdStrat,
				dampening: getStrategyFromTags(tags, "atc.dampening="),
			}
		}
	}
	redirectStrategies := r.redirectStrategies
	dampeningPeriod := r.dampeningPeriod
	minDampeningPeriod := r.minDampeningPeriod
	r.mu.Unlock()

	// 2. Reconcile active catalog services
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			if existing, exists := existingResolverEntries[svcName]; exists {
				if existing.Meta != nil && existing.Meta["created-by"] == "atc-override" {
					r.logger.DebugContext(ctx, "Skipping reconciliation because service-resolver has an active manual override", slog.String("service", svcName))
					continue
				}
			}

			// Cancel any pending writes in forwarder
			if r.forwarder != nil && !r.forwarderEnabled {
				r.forwarder.CancelPendingWrite(ctx, svcName)
			}

			// Determine failover strategy name
			failoverStrategyName := getStrategyFromTags(tags, "atc.failover=")
			if failoverStrategyName == "" && r.hasDefaultFailover {
				failoverStrategyName = "default"
			}

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
				entry := &api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: svcName,
					Meta: map[string]string{
						"created-by":        "atc",
						"failover-strategy": failoverStrategyName,
					},
				}

				dampening := getDampeningDuration(tags, dampeningPeriod, minDampeningPeriod)
				if dampening <= 0 {
					r.CancelPendingWrite(ctx, svcName)
					if r.dryRun {
						span.AddEvent("dry_run_set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "redirect-base")))
						r.logger.InfoContext(ctx, "[DRY RUN] Would create/update base service-resolver immediately", slog.String("service", svcName))
					} else {
						span.AddEvent("set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "redirect-base")))
						r.logger.InfoContext(ctx, "Creating/Updating base service-resolver immediately", slog.String("service", svcName))
						err = r.traceConsulCall(ctx, "set_config_entry", func() error {
							_, _, err := client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
							return err
						})
						if err != nil {
							r.logger.ErrorContext(ctx, "Failed to set base service-resolver", slog.String("service", svcName), slog.Any("error", err))
						}
					}
				} else {
					r.logger.InfoContext(ctx, "Scheduling base service-resolver write", slog.String("service", svcName), slog.Duration("dampening", dampening))
					r.scheduleWrite(svcName, entry, dampening)
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
					if r.dryRun {
						span.AddEvent("dry_run_delete_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "redirect-base")))
						r.logger.InfoContext(ctx, "[DRY RUN] Would delete service-resolver because atc.enabled tag was removed", slog.String("service", svcName))
					} else {
						span.AddEvent("delete_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "redirect-base")))
						r.logger.InfoContext(ctx, "Deleting service-resolver because atc.enabled tag was removed", slog.String("service", svcName))
						err = r.traceConsulCall(ctx, "delete_config_entry", func() error {
							_, err := client.ConfigEntries().Delete("service-resolver", svcName, (&api.WriteOptions{}).WithContext(ctx))
							return err
						})
						if err != nil {
							r.logger.ErrorContext(ctx, "Failed to delete service-resolver", slog.String("service", svcName), slog.Any("error", err))
						}
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
			if existing.Meta != nil && existing.Meta["created-by"] == "atc-override" {
				continue
			}
			if existing.Meta != nil && existing.Meta["created-by"] == "atc" {
				// Cancel any pending writes in forwarder
				if r.forwarder != nil {
					r.forwarder.CancelPendingWrite(ctx, name)
				}

				// First check in-memory cache for redirect-strategy and dampening tag
				var redirectStrategyName string
				var dampeningTag string
				r.mu.RLock()
				if c, ok := r.strategiesCache[name]; ok {
					redirectStrategyName = c.redirect
					dampeningTag = c.dampening
				}
				r.mu.RUnlock()

				if redirectStrategyName == "" {
					// Fallback to existing metadata
					redirectStrategyName = existing.Meta["redirect-strategy"]
				}

				// Default values
				targetSvc := name
				targetDCVal := targetDC
				var targetNamespace string
				var targetServiceSubset string

				if redirectStrategyName != "" {
					if strategy, ok := redirectStrategies[redirectStrategyName]; ok {
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

					dampening := getDampeningDuration([]string{"atc.dampening=" + dampeningTag}, dampeningPeriod, minDampeningPeriod)
					if dampening <= 0 {
						r.CancelPendingWrite(ctx, name)
						if r.dryRun {
							span.AddEvent("dry_run_set_config", trace.WithAttributes(attribute.String("service", name), attribute.String("type", "redirect")))
							r.logger.InfoContext(ctx, "[DRY RUN] Would create/update redirect service-resolver immediately", slog.String("service", name), slog.String("redirect-strategy", redirectStrategyName))
						} else {
							span.AddEvent("set_config", trace.WithAttributes(attribute.String("service", name), attribute.String("type", "redirect")))
							r.logger.InfoContext(ctx, "Creating/Updating redirect service-resolver immediately", slog.String("service", name), slog.String("redirect-strategy", redirectStrategyName))
							err = r.traceConsulCall(ctx, "set_config_entry", func() error {
								_, _, err := client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
								return err
							})
							if err != nil {
								r.logger.ErrorContext(ctx, "Failed to set redirect service-resolver", slog.String("service", name), slog.Any("error", err))
							}
						}
					} else {
						r.logger.InfoContext(ctx, "Scheduling redirect service-resolver write", slog.String("service", name), slog.Duration("dampening", dampening))
						r.scheduleWrite(name, entry, dampening)
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

func (r *Redirector) scheduleWrite(svcName string, entry *api.ServiceResolverConfigEntry, dampening time.Duration) {
	r.schedMu.Lock()
	defer r.schedMu.Unlock()

	if r.dampeningTriggered != nil {
		r.dampeningTriggered.Add(context.Background(), 1)
	}

	if timer, ok := r.writeTimers[svcName]; ok {
		if !entriesEqual(r.pendingWrites[svcName], entry) {
			r.logger.Info("Resetting dampening timer for pending write", slog.String("service", svcName), slog.Duration("dampening", dampening))
			r.pendingWrites[svcName] = entry
			timer.Reset(dampening)
		}
	} else {
		r.pendingWrites[svcName] = entry
		r.writeTimers[svcName] = time.AfterFunc(dampening, func() {
			r.executeScheduledWrite(svcName)
		})
	}
}

func (r *Redirector) executeScheduledWrite(svcName string) {
	r.schedMu.Lock()
	entry, ok := r.pendingWrites[svcName]
	delete(r.pendingWrites, svcName)
	delete(r.writeTimers, svcName)
	r.schedMu.Unlock()

	if !ok || entry == nil {
		return
	}

	ctx, span := tracer.Start(context.Background(), "executeScheduledWrite")
	defer span.End()

	client, err := api.NewClient(consulCfg(r.consulAddr, r.consulToken, r.consulDC, r.consulNamespace))
	if err != nil {
		r.logger.Error("Failed to create Consul client for scheduled write", slog.String("service", svcName), slog.Any("error", err))
		span.RecordError(err)
		return
	}

	if r.dryRun {
		span.AddEvent("dry_run_scheduled_set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "redirect")))
		r.logger.Info("[DRY RUN] Would execute scheduled redirect write for service-resolver", slog.String("service", svcName))
	} else {
		span.AddEvent("scheduled_set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "redirect")))
		r.logger.Info("Executing scheduled redirect write for service-resolver", slog.String("service", svcName))
		err = r.traceConsulCall(ctx, "set_config_entry", func() error {
			_, _, err := client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
			return err
		})
		if err != nil {
			r.logger.Error("Failed to write scheduled redirect service-resolver entry", slog.String("service", svcName), slog.Any("error", err))
			span.RecordError(err)
		}
	}
}

func (r *Redirector) traceConsulCall(ctx context.Context, op string, fn func() error) error {
	ctx, span := tracer.Start(ctx, "consul/"+op, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	start := time.Now()
	err := fn()
	r.recordConsulCall(ctx, op, start, err)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (r *Redirector) recordConsulCall(ctx context.Context, op string, start time.Time, err error) {
	status := "success"
	if err != nil {
		status = "failure"
	}
	if r.consulRequestsCounter != nil {
		r.consulRequestsCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("operation", op),
			attribute.String("status", status),
		))
	}
	if r.consulDurationHistogram != nil {
		r.consulDurationHistogram.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
			attribute.String("operation", op),
		))
	}
}

func (r *Redirector) UpdateConfig(redirectStrategies map[string]RedirectStrategy, dampeningPeriod, minDampeningPeriod, consulNamespace, writeRateLimit string, dryRun bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.redirectStrategies = redirectStrategies
	r.dampeningPeriod = dampeningPeriod
	r.minDampeningPeriod = minDampeningPeriod
	r.consulNamespace = consulNamespace
	r.writeRateLimit = writeRateLimit
	r.dryRun = dryRun
	r.logger.Info("Redirector configuration reloaded dynamically",
		slog.Int("strategies_count", len(redirectStrategies)),
		slog.String("dampening_period", dampeningPeriod),
		slog.String("min_dampening_period", minDampeningPeriod),
		slog.String("consul_namespace", consulNamespace),
		slog.String("write_rate_limit", writeRateLimit),
		slog.Bool("dry_run", dryRun),
	)
}
