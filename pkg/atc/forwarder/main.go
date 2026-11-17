package forwarder

import (
	"context"
	"errors"
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
	tracer = otel.Tracer("atc/forwarder")
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
	CancelPendingWrite(ctx context.Context, svcName string)
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
	consulClient    *api.Client
	resyncInterval  time.Duration
	foreign         *ForeignTracker

	unknownStrategyWarn *WarnTracker
	aclFilteredWarn     *WarnTracker
	noTargetWarn        *WarnTracker

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
	redirector     writeCanceler

	reconcileCounter        metric.Int64Counter
	reconcileDuration       metric.Float64Histogram
	consulRequestsCounter   metric.Int64Counter
	consulDurationHistogram metric.Float64Histogram
	coalescedEventsCounter  metric.Int64Counter
	dampeningTriggered      metric.Int64Counter
	dampeningCancelled      metric.Int64Counter
}

func New(logger *slog.Logger, consulAddr, consulToken, consulDC, consulNamespace, writeRateLimit string, failoverStrategies map[string]FailoverStrategy, dampeningPeriod, minDampeningPeriod string, dryRun bool) (*Forwarder, error) {
	meter := otel.Meter("atc/forwarder")
	reconcileCounter, err := meter.Int64Counter(
		"atc_forwarder_reconcile_runs_total",
		metric.WithDescription("Total number of forwarder reconciliation runs"),
	)
	if err != nil {
		return nil, err
	}
	reconcileDuration, err := meter.Float64Histogram(
		"atc_forwarder_reconcile_duration_seconds",
		metric.WithDescription("Duration of forwarder reconciliation runs in seconds"),
	)
	if err != nil {
		return nil, err
	}

	consulRequestsCounter, err := meter.Int64Counter(
		"atc_consul_requests_total",
		metric.WithDescription("Total number of Consul API requests from forwarder"),
	)
	if err != nil {
		return nil, err
	}

	consulDurationHistogram, err := meter.Float64Histogram(
		"atc_consul_request_duration_seconds",
		metric.WithDescription("Duration of Consul API requests from forwarder in seconds"),
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

	f := &Forwarder{
		resyncInterval:          DefaultResyncInterval,
		logger:                  logger,
		consulAddr:              consulAddr,
		consulToken:             consulToken,
		consulDC:                consulDC,
		consulNamespace:         consulNamespace,
		writeRateLimit:          writeRateLimit,
		watcher:                 watcher.New(logger, consulAddr, consulToken, consulDC, consulNamespace),
		failoverStrategies:      failoverStrategies,
		dampeningPeriod:         dampeningPeriod,
		minDampeningPeriod:      minDampeningPeriod,
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
			f.schedMu.Lock()
			val := int64(len(f.pendingWrites))
			f.schedMu.Unlock()
			obsrv.Observe(val, metric.WithAttributes(attribute.String("module", "forwarder")))
			return nil
		}),
	)
	if err != nil {
		return nil, err
	}

	f.foreign = NewForeignTracker(meter, "forwarder", logger)
	f.aclFilteredWarn = NewWarnTracker(logger, "Consul filtered the service catalog by ACLs: services the token cannot read are invisible to ATC")
	f.unknownStrategyWarn = NewWarnTracker(logger, "Service tag names a failover strategy that is not configured; using the default target")
	f.noTargetWarn = NewWarnTracker(logger, "Not writing a failover resolver: only one datacenter is known")
	return f, nil
}

func (f *Forwarder) SetWatcher(w *watcher.ConsulWatcher) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watcher = w
}

func (f *Forwarder) getConsulClient() (*api.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consulClient != nil {
		return f.consulClient, nil
	}
	client, err := api.NewClient(consulCfg(f.consulAddr, f.consulToken, f.consulDC, f.consulNamespace))
	if err != nil {
		return nil, err
	}
	f.consulClient = client
	return client, nil
}

func (f *Forwarder) SetRedirector(r writeCanceler) {
	f.schedMu.Lock()
	defer f.schedMu.Unlock()
	f.redirector = r
}

func (f *Forwarder) CancelPendingWrite(ctx context.Context, svcName string) {
	f.schedMu.Lock()
	defer f.schedMu.Unlock()
	if timer, ok := f.writeTimers[svcName]; ok {
		f.logger.Info("Cancelled pending service-resolver write", slog.String("service", svcName), slog.String("reason", "reconciliation_canceled"))

		span := trace.SpanFromContext(ctx)
		span.AddEvent("dampening_cancelled", trace.WithAttributes(
			attribute.String("service", svcName),
			attribute.String("reason", "reconciliation_canceled"),
		))

		if f.dampeningCancelled != nil {
			f.dampeningCancelled.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "cancelled")))
		}
		timer.Stop()
		delete(f.writeTimers, svcName)
	}
	delete(f.pendingWrites, svcName)
	delete(f.pendingOwner, svcName)
}

func (f *Forwarder) CancelAllPendingWrites(ctx context.Context) {
	f.schedMu.Lock()
	defer f.schedMu.Unlock()
	for svcName, timer := range f.writeTimers {
		timer.Stop()
		delete(f.writeTimers, svcName)
		if f.dampeningCancelled != nil {
			f.dampeningCancelled.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "shutdown")))
		}
	}
	clear(f.pendingWrites)
	clear(f.pendingOwner)
}

func (f *Forwarder) GetCachedStrategies(svcName string) (string, string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if c, ok := f.strategiesCache[svcName]; ok {
		return c.failover, c.redirect
	}
	return "", ""
}

// stopScheduledWrites ends the current leadership term: it prevents any timer that has
// already fired from writing, cancels pending timers, and waits for writes in flight.
// After it returns this instance will not write to Consul until Run is called again.
func (f *Forwarder) stopScheduledWrites(ctx context.Context) {
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	f.schedMu.Lock()
	f.runCtx = stopped
	f.schedMu.Unlock()
	f.CancelAllPendingWrites(ctx)
	f.inflightWrites.Wait()
}

// reconcileWithCurrentClient reconciles with the current Consul client, so connection
// changes applied through SetConnection or UpdateConfig take effect on the next run.
func (f *Forwarder) reconcileWithCurrentClient(ctx context.Context) error {
	client, err := f.getConsulClient()
	if err != nil {
		return fmt.Errorf("failed to create consul client: %w", err)
	}
	return f.reconcile(ctx, client)
}

// DefaultResyncInterval is how often a running module reconciles even without catalog events.
const DefaultResyncInterval = 60 * time.Second

// SetResyncInterval sets the periodic resync interval (0 disables it). It takes effect when
// Run next starts.
func (f *Forwarder) SetResyncInterval(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resyncInterval = d
}

// SetConnection updates the Consul connection settings. When they change, the cached client
// is dropped so the next reconciliation and scheduled write use the new settings.
func (f *Forwarder) SetConnection(addr, token, dc, ns string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.consulAddr == addr && f.consulToken == token && f.consulDC == dc && f.consulNamespace == ns {
		return
	}
	f.consulAddr, f.consulToken, f.consulDC, f.consulNamespace = addr, token, dc, ns
	f.consulClient = nil
	f.logger.Info("Consul connection settings changed", slog.String("consul_addr", addr), slog.String("consul_dc", dc), slog.String("consul_namespace", ns))
}

func (f *Forwarder) Run(ctx context.Context) error {
	f.schedMu.Lock()
	f.runCtx = ctx
	f.schedMu.Unlock()
	defer f.stopScheduledWrites(ctx)

	if _, err := f.getConsulClient(); err != nil {
		return fmt.Errorf("failed to create consul client: %w", err)
	}

	f.mu.RLock()
	w := f.watcher
	resyncInterval := f.resyncInterval
	f.mu.RUnlock()

	events := w.Events.Subscribe()
	defer w.Events.Unsubscribe(events)

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return w.Run(ctx)
	})

	g.Go(func() error {
		f.logger.InfoContext(ctx, "Starting initial forwarder catalog reconciliation")
		if err := f.reconcileWithCurrentClient(ctx); err != nil {
			f.logger.ErrorContext(ctx, "Initial reconciliation failed", slog.Any("error", err))
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
					if err := f.reconcileWithCurrentClient(ctx); err != nil {
						f.logger.ErrorContext(ctx, "Reconciliation failed", slog.Any("error", err))
					}
				} else {
					if debounceTimer != nil {
						debounceTimer.Stop()
					}
					f.logger.InfoContext(ctx, "Coalescing/debouncing watcher events, scheduling reconciliation", slog.Duration("window", rateLimit))
					if f.coalescedEventsCounter != nil {
						f.coalescedEventsCounter.Add(ctx, 1)
					}
					debounceTimer = time.NewTimer(rateLimit)
					timerChan = debounceTimer.C
				}

			case <-resyncChan:
				f.logger.DebugContext(ctx, "Periodic resync, running reconciliation")
				if err := f.reconcileWithCurrentClient(ctx); err != nil {
					f.logger.ErrorContext(ctx, "Periodic reconciliation failed", slog.Any("error", err))
				}

			case <-timerChan:
				timerChan = nil
				f.logger.InfoContext(ctx, "Coalescing window elapsed, running reconciliation")
				if err := f.reconcileWithCurrentClient(ctx); err != nil {
					f.logger.ErrorContext(ctx, "Reconciliation failed", slog.Any("error", err))
				}
			}
		}
	})

	return g.Wait()
}

func (f *Forwarder) reconcile(ctx context.Context, client *api.Client) (err error) {
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
		f.reconcileCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("status", status),
		))
		f.reconcileDuration.Record(ctx, duration, metric.WithAttributes(
			attribute.String("status", status),
		))
	}()

	var services map[string][]string
	var servicesMeta *api.QueryMeta
	err = f.traceConsulCall(ctx, "list_services", func() error {
		var err error
		services, servicesMeta, err = client.Catalog().Services((&api.QueryOptions{}).WithContext(ctx))
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to fetch consul services: %w", err)
	}
	// Consul hides what the token may not read instead of failing, so a token without
	// service:read / node:read makes ATC see no services and silently do nothing.
	if servicesMeta != nil && servicesMeta.ResultsFilteredByACLs {
		f.aclFilteredWarn.Update(ctx, map[string]string{"catalog": "the Consul token cannot read every service; see the ATC ACL policy"})
	} else {
		f.aclFilteredWarn.Update(ctx, nil)
	}

	f.mu.RLock()
	configDC := f.consulDC
	f.mu.RUnlock()
	localDC, err := LocalDatacenter(client, configDC)
	if err != nil {
		return err
	}
	remoteDCs, err := RemoteDatacenters(client, localDC)
	if err != nil {
		return err
	}

	// 1. Fetch all existing config entries of type service-resolver
	var entries []api.ConfigEntry
	err = f.traceConsulCall(ctx, "list_config_entries", func() error {
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
	dryRun := f.dryRun
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
				f.redirector.CancelPendingWrite(ctx, svcName)
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

			// Datacenters chosen automatically for this service before, by target position,
			// so the choice stays stable while those DCs remain available.
			var previousDCs []string
			if prev, ok := existingResolverEntries[svcName]; ok && IsATCOwned(prev) {
				if fo, ok := prev.Failover["*"]; ok {
					for _, t := range fo.Targets {
						previousDCs = append(previousDCs, t.Datacenter)
					}
				}
			}
			autoDC := func(i int) string {
				preferred := ""
				if i < len(previousDCs) {
					preferred = previousDCs[i]
				}
				return PickDatacenter(remoteDCs, preferred)
			}

			if failoverStrategyName != "" {
				if _, ok := failoverStrategies[failoverStrategyName]; !ok {
					unknownStrategy[svcName] = failoverStrategyName
				}
				if strategy, ok := failoverStrategies[failoverStrategyName]; ok {
					if strategy.ConnectTimeout != "" {
						if d, err := time.ParseDuration(strategy.ConnectTimeout); err == nil {
							connectTimeout = d
						}
					}
					for i, target := range strategy.Targets {
						tSvc := target.Service
						if tSvc == "" {
							tSvc = svcName
						}
						tDC := target.Datacenter
						if tDC == "" {
							tDC = autoDC(i)
						}
						if tDC == "" {
							continue // no other datacenter to fail over to
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
				if dc := autoDC(0); dc != "" {
					targets = []api.ServiceResolverFailoverTarget{{Service: svcName, Datacenter: dc}}
				}
			}
			if len(targets) == 0 {
				// Only one datacenter is known: writing a failover to a made-up DC (as before)
				// would be wrong, so leave the service alone.
				noTarget[svcName] = "no other datacenter to fail over to"
				continue
			}

			// Record what should happen if the service later disappears from the catalog, so any
			// leader can apply the right redirect even if it never saw this service's tags.
			removalIntent := RemovalIntentMeta(tags)

			existing, exists := existingResolverEntries[svcName]
			if !MayManage(existing, exists, tags) {
				foreign[svcName] = struct{}{}
				continue
			}
			needsCreateOrUpdate := !exists ||
				existing.Meta == nil ||
				existing.Meta["created-by"] != "atc" ||
				existing.Failover == nil ||
				len(existing.Failover) == 0 ||
				existing.Meta["failover-strategy"] != failoverStrategyName ||
				existing.Meta["redirect-strategy"] != "" ||
				!removalIntentMatches(existing.Meta, removalIntent) ||
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
					Meta: withRemovalIntent(map[string]string{
						"created-by":        "atc",
						"failover-strategy": failoverStrategyName,
					}, removalIntent),
					ConnectTimeout: connectTimeout,
					Failover: map[string]api.ServiceResolverFailover{
						"*": {
							Targets: targets,
						},
					},
				}

				dampening := getDampeningDuration(tags, dampeningPeriod, minDampeningPeriod)
				if dampening <= 0 {
					f.CancelPendingWrite(ctx, svcName)
					if dryRun {
						span.AddEvent("dry_run_set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
						f.logger.InfoContext(ctx, "[DRY RUN] Would create/update failover service-resolver immediately", slog.String("service", svcName), slog.String("failover-strategy", failoverStrategyName))
					} else {
						span.AddEvent("set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
						f.logger.InfoContext(ctx, "Creating/Updating failover service-resolver immediately", slog.String("service", svcName), slog.String("failover-strategy", failoverStrategyName))
						err = f.traceConsulCall(ctx, "set_config_entry", func() error {
							_, _, err := client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
							return err
						})
						if err != nil {
							f.logger.ErrorContext(ctx, "Failed to set failover service-resolver", slog.String("service", svcName), slog.Any("error", err))
							writeErrs = append(writeErrs, err)
						}
					}
				} else {
					f.logger.InfoContext(ctx, "Scheduling failover service-resolver write", slog.String("service", svcName), slog.Duration("dampening", dampening))
					f.scheduleWrite(svcName, entry, dampening, OwnerState(existing, exists))
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
						span.AddEvent("dry_run_delete_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
						f.logger.InfoContext(ctx, "[DRY RUN] Would delete service-resolver because atc.enabled tag was removed", slog.String("service", svcName))
					} else {
						span.AddEvent("delete_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
						f.logger.InfoContext(ctx, "Deleting service-resolver because atc.enabled tag was removed", slog.String("service", svcName))
						err = f.traceConsulCall(ctx, "delete_config_entry", func() error {
							_, err := client.ConfigEntries().Delete("service-resolver", svcName, (&api.WriteOptions{}).WithContext(ctx))
							return err
						})
						if err != nil {
							f.logger.ErrorContext(ctx, "Failed to delete service-resolver", slog.String("service", svcName), slog.Any("error", err))
							writeErrs = append(writeErrs, err)
						}
					}
				}
			}
		}
	}

	f.foreign.Update(ctx, foreign)
	f.unknownStrategyWarn.Update(ctx, unknownStrategy)
	f.noTargetWarn.Update(ctx, noTarget)
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

func (f *Forwarder) scheduleWrite(svcName string, entry *api.ServiceResolverConfigEntry, dampening time.Duration, owner string) {
	f.schedMu.Lock()
	defer f.schedMu.Unlock()

	if f.dampeningTriggered != nil {
		f.dampeningTriggered.Add(context.Background(), 1)
	}

	if timer, ok := f.writeTimers[svcName]; ok {
		if !entriesEqual(f.pendingWrites[svcName], entry) {
			f.logger.Info("Resetting dampening timer for pending write", slog.String("service", svcName), slog.Duration("dampening", dampening))
			f.pendingWrites[svcName] = entry
			f.pendingOwner[svcName] = owner
			timer.Reset(dampening)
		}
	} else {
		f.pendingWrites[svcName] = entry
		f.pendingOwner[svcName] = owner
		f.writeTimers[svcName] = time.AfterFunc(dampening, func() {
			f.executeScheduledWrite(svcName)
		})
	}
}

func (f *Forwarder) executeScheduledWrite(svcName string) {
	f.schedMu.Lock()
	entry, ok := f.pendingWrites[svcName]
	owner, haveOwner := f.pendingOwner[svcName]
	delete(f.pendingOwner, svcName)
	delete(f.pendingWrites, svcName)
	delete(f.pendingOwner, svcName)
	delete(f.writeTimers, svcName)
	parent := f.runCtx
	if parent == nil {
		parent = context.Background()
	}
	if parent.Err() != nil {
		// The leadership term this write was scheduled in has ended; never write stale state.
		f.schedMu.Unlock()
		if ok {
			f.logger.Info("Dropping scheduled failover write: no longer running as leader", slog.String("service", svcName))
		}
		return
	}
	if ok && entry != nil {
		f.inflightWrites.Add(1)
		defer f.inflightWrites.Done()
	}
	f.schedMu.Unlock()

	if !ok || entry == nil {
		return
	}

	ctx, span := tracer.Start(parent, "executeScheduledWrite")
	defer span.End()

	client, err := f.getConsulClient()
	if err != nil {
		f.logger.Error("Failed to create Consul client for scheduled write", slog.String("service", svcName), slog.Any("error", err))
		span.RecordError(err)
		return
	}

	// The resolver may have changed during the dampening window: purged (possibly through
	// another ATC node), replaced by a manual override, or written by someone else. Then this
	// write is stale; drop it and let the next reconcile decide with current state.
	if haveOwner {
		current, err := CurrentOwnerState(ctx, client, svcName)
		if err != nil {
			f.logger.Error("Dropping scheduled failover write: could not re-check the resolver; the next resync retries", slog.String("service", svcName), slog.Any("error", err))
			span.RecordError(err)
			return
		}
		if current != owner {
			f.logger.Info("Dropping scheduled failover write: the resolver changed while the write was pending", slog.String("service", svcName), slog.String("was", owner), slog.String("now", current))
			span.AddEvent("scheduled_write_dropped_resolver_changed")
			return
		}
	}

	f.mu.RLock()
	dryRun := f.dryRun
	f.mu.RUnlock()

	if dryRun {
		span.AddEvent("dry_run_scheduled_set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
		f.logger.Info("[DRY RUN] Would execute scheduled failover write for service-resolver", slog.String("service", svcName))
	} else {
		span.AddEvent("scheduled_set_config", trace.WithAttributes(attribute.String("service", svcName), attribute.String("type", "failover")))
		f.logger.Info("Executing scheduled failover write for service-resolver", slog.String("service", svcName))
		err = f.traceConsulCall(ctx, "set_config_entry", func() error {
			_, _, err := client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
			return err
		})
		if err != nil {
			f.logger.Error("Failed to write scheduled failover service-resolver entry", slog.String("service", svcName), slog.Any("error", err))
			span.RecordError(err)
		}
	}
}

func (f *Forwarder) traceConsulCall(ctx context.Context, op string, fn func() error) error {
	ctx, span := tracer.Start(ctx, "consul/"+op, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	start := time.Now()
	err := fn()
	f.recordConsulCall(ctx, op, start, err)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

func (f *Forwarder) recordConsulCall(ctx context.Context, op string, start time.Time, err error) {
	status := "success"
	if err != nil {
		status = "failure"
	}
	if f.consulRequestsCounter != nil {
		f.consulRequestsCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("operation", op),
			attribute.String("status", status),
		))
	}
	if f.consulDurationHistogram != nil {
		f.consulDurationHistogram.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
			attribute.String("operation", op),
		))
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
	if f.consulNamespace != consulNamespace {
		f.consulClient = nil
		if f.watcher != nil {
			f.watcher.SetNamespace(consulNamespace)
		}
	}
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

// Metadata keys that persist a service's redirect intent in its resolver entry. They are
// distinct from "redirect-strategy", which marks an entry that IS a redirect.
const (
	MetaRedirectOnRemoval  = "atc-redirect-on-removal"
	MetaDampeningOnRemoval = "atc-dampening-on-removal"
)

// RemovalIntentMeta extracts the redirect and dampening tags that apply when the service is
// removed from the catalog. Empty values are omitted.
func RemovalIntentMeta(tags []string) map[string]string {
	intent := map[string]string{}
	if v := getStrategyFromTags(tags, "atc.redirect="); v != "" {
		intent[MetaRedirectOnRemoval] = v
	}
	if v := getStrategyFromTags(tags, "atc.dampening="); v != "" {
		intent[MetaDampeningOnRemoval] = v
	}
	return intent
}

func removalIntentMatches(meta, intent map[string]string) bool {
	return meta[MetaRedirectOnRemoval] == intent[MetaRedirectOnRemoval] &&
		meta[MetaDampeningOnRemoval] == intent[MetaDampeningOnRemoval]
}

func withRemovalIntent(meta, intent map[string]string) map[string]string {
	for k, v := range intent {
		meta[k] = v
	}
	return meta
}
