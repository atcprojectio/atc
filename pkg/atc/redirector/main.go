package redirector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/atcprojectio/atc/pkg/atc/forwarder"
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

	mu             sync.RWMutex
	consulClient   *api.Client
	resyncInterval time.Duration
	foreign        *forwarder.ForeignTracker

	unknownStrategyWarn *forwarder.WarnTracker
	aclFilteredWarn     *forwarder.WarnTracker
	noTargetWarn        *forwarder.WarnTracker
	strategiesCache     map[string]cachedStrategies

	// Hysteresis scheduling state
	schedMu       sync.Mutex
	pendingWrites map[string]*api.ServiceResolverConfigEntry
	// pendingOwner records who controlled each resolver when its write was scheduled; the
	// write is dropped if that changed before it fires (purged, overridden, taken over).
	pendingOwner map[string]string
	writeTimers  map[string]*time.Timer
	// runCtx is the context of the current Run (i.e. the leadership term). Scheduled writes
	// derive from it so they cannot outlive a step-down. nil means Run was never started.
	runCtx context.Context
	// inflightWrites tracks scheduled writes that have started, so Run can wait for them.
	inflightWrites sync.WaitGroup
	forwarder      writeCanceler

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
		resyncInterval:          DefaultResyncInterval,
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
		pendingOwner:            make(map[string]string),
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

	r.foreign = forwarder.NewForeignTracker(meter, "redirector", logger)
	r.aclFilteredWarn = forwarder.NewWarnTracker(logger, "Consul filtered the service catalog by ACLs: services the token cannot read are invisible to ATC")
	r.unknownStrategyWarn = forwarder.NewWarnTracker(logger, "Service tag names a redirect strategy that is not configured; the default target will be used if the service disappears")
	r.noTargetWarn = forwarder.NewWarnTracker(logger, "Not writing a redirect resolver: only one datacenter is known")
	return r, nil
}

func (r *Redirector) SetForwarder(f writeCanceler) {
	r.schedMu.Lock()
	defer r.schedMu.Unlock()
	r.forwarder = f
}

func (r *Redirector) SetWatcher(w *watcher.ConsulWatcher) {
	if w == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watcher = w
}

func (r *Redirector) SetHasDefaultFailover(has bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hasDefaultFailover = has
}

func (r *Redirector) getConsulClient() (*api.Client, error) {
	r.mu.RLock()
	client := r.consulClient
	addr, token, dc, ns := r.consulAddr, r.consulToken, r.consulDC, r.consulNamespace
	r.mu.RUnlock()
	if client != nil {
		return client, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.consulClient != nil {
		return r.consulClient, nil
	}
	c, err := api.NewClient(consulCfg(addr, token, dc, ns))
	if err != nil {
		return nil, err
	}
	r.consulClient = c
	return c, nil
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
	delete(r.pendingOwner, svcName)
}

func (r *Redirector) CancelAllPendingWrites(ctx context.Context) {
	r.schedMu.Lock()
	defer r.schedMu.Unlock()
	for svcName, timer := range r.writeTimers {
		r.logger.Info("Cancelled pending service-resolver write on shutdown/stepdown", slog.String("service", svcName))
		if r.dampeningCancelled != nil {
			r.dampeningCancelled.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "shutdown")))
		}
		timer.Stop()
		delete(r.writeTimers, svcName)
		delete(r.pendingWrites, svcName)
		delete(r.pendingOwner, svcName)
		delete(r.pendingOwner, svcName)
	}
}

func (r *Redirector) GetCachedStrategies(svcName string) (string, string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if c, ok := r.strategiesCache[svcName]; ok {
		return c.failover, c.redirect
	}
	return "", ""
}

// stopScheduledWrites ends the current leadership term: it prevents any timer that has
// already fired from writing, cancels pending timers, and waits for writes in flight.
// After it returns this instance will not write to Consul until Run is called again.
func (r *Redirector) stopScheduledWrites(ctx context.Context) {
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	r.schedMu.Lock()
	r.runCtx = stopped
	r.schedMu.Unlock()
	r.CancelAllPendingWrites(ctx)
	r.inflightWrites.Wait()
}

// reconcileWithCurrentClient reconciles with the current Consul client, so connection
// changes applied through SetConnection or UpdateConfig take effect on the next run.
func (r *Redirector) reconcileWithCurrentClient(ctx context.Context) error {
	client, err := r.getConsulClient()
	if err != nil {
		return fmt.Errorf("failed to create consul client: %w", err)
	}
	return r.reconcile(ctx, client)
}

// DefaultResyncInterval is how often a running module reconciles even without catalog events.
const DefaultResyncInterval = 60 * time.Second

// SetResyncInterval sets the periodic resync interval (0 disables it). It takes effect when
// Run next starts.
func (r *Redirector) SetResyncInterval(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resyncInterval = d
}

// SetConnection updates the Consul connection settings. When they change, the cached client
// is dropped so the next reconciliation and scheduled write use the new settings.
func (r *Redirector) SetConnection(addr, token, dc, ns string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.consulAddr == addr && r.consulToken == token && r.consulDC == dc && r.consulNamespace == ns {
		return
	}
	r.consulAddr, r.consulToken, r.consulDC, r.consulNamespace = addr, token, dc, ns
	r.consulClient = nil
	r.logger.Info("Consul connection settings changed", slog.String("consul_addr", addr), slog.String("consul_dc", dc), slog.String("consul_namespace", ns))
}

func (r *Redirector) Run(ctx context.Context) error {
	r.schedMu.Lock()
	r.runCtx = ctx
	r.schedMu.Unlock()
	defer r.stopScheduledWrites(ctx)

	if _, err := r.getConsulClient(); err != nil {
		return fmt.Errorf("failed to create consul client: %w", err)
	}

	r.mu.RLock()
	w := r.watcher
	resyncInterval := r.resyncInterval
	r.mu.RUnlock()

	events := w.Events.Subscribe()
	defer w.Events.Unsubscribe(events)

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return w.Run(ctx)
	})

	g.Go(func() error {
		r.logger.InfoContext(ctx, "Starting initial redirector catalog reconciliation")
		if err := r.reconcileWithCurrentClient(ctx); err != nil {
			r.logger.ErrorContext(ctx, "Initial reconciliation failed", slog.Any("error", err))
		}

		var (
			debounceTimer *time.Timer
			timerChan     <-chan time.Time
			resyncChan    <-chan time.Time
		)

		// Periodic full resync: retries failed writes and repairs drift that no catalog
		// event would reveal (e.g. a resolver deleted out-of-band or an expired override).
		if resyncInterval > 0 {
			resyncTicker := time.NewTicker(resyncInterval)
			defer resyncTicker.Stop()
			resyncChan = resyncTicker.C
		}

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
					if err := r.reconcileWithCurrentClient(ctx); err != nil {
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

			case <-resyncChan:
				r.logger.DebugContext(ctx, "Periodic resync, running reconciliation")
				if err := r.reconcileWithCurrentClient(ctx); err != nil {
					r.logger.ErrorContext(ctx, "Periodic reconciliation failed", slog.Any("error", err))
				}

			case <-timerChan:
				timerChan = nil
				r.logger.InfoContext(ctx, "Coalescing window elapsed, running reconciliation")
				if err := r.reconcileWithCurrentClient(ctx); err != nil {
					r.logger.ErrorContext(ctx, "Reconciliation failed", slog.Any("error", err))
				}
			}
		}
	})

	return g.Wait()
}

func (r *Redirector) reconcile(ctx context.Context, client *api.Client) (err error) {
	ctx, span := tracer.Start(ctx, "reconcile")
	// Failed writes are collected and returned so the run is counted as failed and the
	// next periodic resync retries them, instead of being logged and forgotten.
	var writeErrs []error
	foreign := map[string]struct{}{}
	unknownStrategy := map[string]string{}
	noTarget := map[string]string{}
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
	var servicesMeta *api.QueryMeta
	err = r.traceConsulCall(ctx, "list_services", func() error {
		var err error
		services, servicesMeta, err = client.Catalog().Services((&api.QueryOptions{}).WithContext(ctx))
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to fetch consul services: %w", err)
	}
	// Consul hides what the token may not read instead of failing, so a token without
	// service:read / node:read makes ATC see no services and silently do nothing.
	if !r.forwarderEnabled && servicesMeta != nil && servicesMeta.ResultsFilteredByACLs {
		r.aclFilteredWarn.Update(ctx, map[string]string{"catalog": "the Consul token cannot read every service; see the ATC ACL policy"})
	} else {
		r.aclFilteredWarn.Update(ctx, nil)
	}

	r.mu.RLock()
	configDC := r.consulDC
	r.mu.RUnlock()
	localDC, err := forwarder.LocalDatacenter(client, configDC)
	if err != nil {
		return err
	}
	remoteDCs, err := forwarder.RemoteDatacenters(client, localDC)
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
	hasDefaultFailover := r.hasDefaultFailover
	dryRun := r.dryRun
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			foStrat := getStrategyFromTags(tags, "atc.failover=")
			if foStrat == "" && hasDefaultFailover {
				foStrat = "default"
			}
			rdStrat := getStrategyFromTags(tags, "atc.redirect=")
			if rdStrat != "" {
				if _, ok := r.redirectStrategies[rdStrat]; !ok {
					// Warn while the service is still running, not when the redirect is needed.
					unknownStrategy[svcName] = rdStrat
				}
			}
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
			if failoverStrategyName == "" && hasDefaultFailover {
				failoverStrategyName = "default"
			}

			removalIntent := forwarder.RemovalIntentMeta(tags)

			existing, exists := existingResolverEntries[svcName]
			// With the forwarder running, the redirector writes nothing for active services, so
			// only the forwarder reports resolvers it leaves alone (one warning, one gauge value).
			if !r.forwarderEnabled && !forwarder.MayManage(existing, exists, tags) {
				foreign[svcName] = struct{}{}
				continue
			}
			var needsSet bool
			if r.forwarderEnabled {
				needsSet = false
			} else {
				needsSet = !exists ||
					existing.Meta == nil ||
					existing.Meta["created-by"] != "atc" ||
					(existing.Redirect != nil && existing.Redirect.Service != "") ||
					existing.Meta["failover-strategy"] != failoverStrategyName ||
					existing.Meta["redirect-strategy"] != "" ||
					existing.Meta[forwarder.MetaRedirectOnRemoval] != removalIntent[forwarder.MetaRedirectOnRemoval] ||
					existing.Meta[forwarder.MetaDampeningOnRemoval] != removalIntent[forwarder.MetaDampeningOnRemoval]
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
				for k, v := range removalIntent {
					entry.Meta[k] = v
				}

				dampening := getDampeningDuration(tags, dampeningPeriod, minDampeningPeriod)
				if dampening <= 0 {
					r.CancelPendingWrite(ctx, svcName)
					if dryRun {
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
							writeErrs = append(writeErrs, err)
						}
					}
				} else {
					r.logger.InfoContext(ctx, "Scheduling base service-resolver write", slog.String("service", svcName), slog.Duration("dampening", dampening))
					r.scheduleWrite(svcName, entry, dampening, forwarder.OwnerState(existing, exists))
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
					if dryRun {
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
							writeErrs = append(writeErrs, err)
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

				// The in-memory cache only knows services this leader has seen. Fall back to what
				// the entry itself records, so a new leader redirects to the configured target.
				if redirectStrategyName == "" {
					redirectStrategyName = existing.Meta["redirect-strategy"] // already a redirect entry
				}
				if redirectStrategyName == "" {
					redirectStrategyName = existing.Meta[forwarder.MetaRedirectOnRemoval]
				}
				if redirectStrategyName == "" {
					if _, ok := redirectStrategies["default"]; ok {
						redirectStrategyName = "default"
					}
				}
				if dampeningTag == "" {
					dampeningTag = existing.Meta[forwarder.MetaDampeningOnRemoval]
				}

				// Default values. A DC chosen automatically before is kept while it is still
				// available, so round-trip-time changes do not flip the redirect.
				previousDC := ""
				if existing.Redirect != nil {
					previousDC = existing.Redirect.Datacenter
				}
				targetSvc := name
				targetDCVal := forwarder.PickDatacenter(remoteDCs, previousDC)
				var targetNamespace string
				var targetServiceSubset string

				if redirectStrategyName != "" {
					if strategy, ok := redirectStrategies[redirectStrategyName]; ok {
						targetSvc = strategy.Service
						if targetSvc == "" {
							targetSvc = name
						}
						if strategy.Datacenter != "" {
							targetDCVal = strategy.Datacenter
						}
						targetNamespace = strategy.Namespace
						targetServiceSubset = strategy.ServiceSubset
					}
				}

				if targetDCVal == "" {
					noTarget[name] = "no other datacenter to redirect to"
					continue
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
						if dryRun {
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
								writeErrs = append(writeErrs, err)
							}
						}
					} else {
						r.logger.InfoContext(ctx, "Scheduling redirect service-resolver write", slog.String("service", name), slog.Duration("dampening", dampening))
						r.scheduleWrite(name, entry, dampening, forwarder.OwnerState(existing, true))
					}
				}
			}
		}
	}

	r.foreign.Update(ctx, foreign)
	// Forget services that are gone from the catalog and have no resolver left: there is
	// nothing to redirect for them any more, and the cache would otherwise only grow.
	r.mu.Lock()
	for name := range r.strategiesCache {
		_, inCatalog := services[name]
		_, hasEntry := existingResolverEntries[name]
		if !inCatalog && !hasEntry {
			delete(r.strategiesCache, name)
		}
	}
	r.mu.Unlock()
	r.unknownStrategyWarn.Update(ctx, unknownStrategy)
	r.noTargetWarn.Update(ctx, noTarget)
	return errors.Join(writeErrs...)
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

func (r *Redirector) scheduleWrite(svcName string, entry *api.ServiceResolverConfigEntry, dampening time.Duration, owner string) {
	r.schedMu.Lock()
	defer r.schedMu.Unlock()

	if r.dampeningTriggered != nil {
		r.dampeningTriggered.Add(context.Background(), 1)
	}

	if timer, ok := r.writeTimers[svcName]; ok {
		if !entriesEqual(r.pendingWrites[svcName], entry) {
			r.logger.Info("Resetting dampening timer for pending write", slog.String("service", svcName), slog.Duration("dampening", dampening))
			r.pendingWrites[svcName] = entry
			r.pendingOwner[svcName] = owner
			timer.Reset(dampening)
		}
	} else {
		r.pendingWrites[svcName] = entry
		r.pendingOwner[svcName] = owner
		r.writeTimers[svcName] = time.AfterFunc(dampening, func() {
			r.executeScheduledWrite(svcName)
		})
	}
}

func (r *Redirector) executeScheduledWrite(svcName string) {
	r.schedMu.Lock()
	entry, ok := r.pendingWrites[svcName]
	owner, haveOwner := r.pendingOwner[svcName]
	delete(r.pendingOwner, svcName)
	delete(r.pendingWrites, svcName)
	delete(r.pendingOwner, svcName)
	delete(r.writeTimers, svcName)
	parent := r.runCtx
	if parent == nil {
		parent = context.Background()
	}
	if parent.Err() != nil {
		// The leadership term this write was scheduled in has ended; never write stale state.
		r.schedMu.Unlock()
		if ok {
			r.logger.Info("Dropping scheduled redirect write: no longer running as leader", slog.String("service", svcName))
		}
		return
	}
	if ok && entry != nil {
		r.inflightWrites.Add(1)
		defer r.inflightWrites.Done()
	}
	r.schedMu.Unlock()

	if !ok || entry == nil {
		return
	}

	ctx, span := tracer.Start(parent, "executeScheduledWrite")
	defer span.End()

	client, err := r.getConsulClient()
	if err != nil {
		r.logger.Error("Failed to create Consul client for scheduled write", slog.String("service", svcName), slog.Any("error", err))
		span.RecordError(err)
		return
	}

	// The resolver may have changed during the dampening window: purged (possibly through
	// another ATC node), replaced by a manual override, or written by someone else. Then this
	// write is stale; drop it and let the next reconcile decide with current state.
	if haveOwner {
		current, err := forwarder.CurrentOwnerState(ctx, client, svcName)
		if err != nil {
			r.logger.Error("Dropping scheduled redirect write: could not re-check the resolver; the next resync retries", slog.String("service", svcName), slog.Any("error", err))
			span.RecordError(err)
			return
		}
		if current != owner {
			r.logger.Info("Dropping scheduled redirect write: the resolver changed while the write was pending", slog.String("service", svcName), slog.String("was", owner), slog.String("now", current))
			span.AddEvent("scheduled_write_dropped_resolver_changed")
			return
		}
	}

	r.mu.RLock()
	dryRun := r.dryRun
	r.mu.RUnlock()

	if dryRun {
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
	if r.consulNamespace != consulNamespace {
		r.consulClient = nil
		if r.watcher != nil {
			r.watcher.SetNamespace(consulNamespace)
		}
	}
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
