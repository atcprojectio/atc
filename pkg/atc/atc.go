package atc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/atcprojectio/atc/pkg/atc/forwarder"
	"github.com/atcprojectio/atc/pkg/atc/redirector"
	atc_server "github.com/atcprojectio/atc/pkg/atc/server"
	"github.com/atcprojectio/atc/pkg/atc/telemetry"
	"github.com/atcprojectio/atc/pkg/atc/watcher"
	"github.com/hashicorp/consul/api"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type StrategiesConfig struct {
	Failover map[string]forwarder.FailoverStrategy  `yaml:"failover" json:"failover" mapstructure:"failover"`
	Redirect map[string]redirector.RedirectStrategy `yaml:"redirect" json:"redirect" mapstructure:"redirect"`
}

type HaConfig struct {
	Enabled    bool   `yaml:"enabled" json:"enabled" mapstructure:"enabled"`
	LockKey    string `yaml:"lock_key" json:"lock_key" mapstructure:"lock_key"`
	SessionTTL string `yaml:"session_ttl" json:"session_ttl" mapstructure:"session_ttl"`
}

type AuthConfig struct {
	Enabled               bool     `yaml:"enabled" json:"enabled" mapstructure:"enabled"`
	StaticKeys            []string `yaml:"static_keys" json:"static_keys" mapstructure:"static_keys"`
	ConsulTokenDelegation bool     `yaml:"consul_token_delegation" json:"consul_token_delegation" mapstructure:"consul_token_delegation"`
	// AllowUnauthenticated explicitly permits running with auth disabled while the API/MCP
	// listeners are reachable from other hosts. Without it, auth must be enabled or
	// server.listen_address must be a loopback address.
	AllowUnauthenticated bool `yaml:"allow_unauthenticated" json:"allow_unauthenticated" mapstructure:"allow_unauthenticated"`
}

type Config struct {
	Name               string            `yaml:"service" mapstructure:"service"`
	NodeName           string            `yaml:"node_name" json:"node_name,omitempty" mapstructure:"node_name"`
	Server             atc_server.Config `yaml:"server" mapstructure:"server"`
	Target             []string          `yaml:"target" mapstructure:"target"`
	ConsulAddr         string            `yaml:"consul_addr" mapstructure:"consul_addr"`
	ConsulToken        string            `yaml:"consul_token" mapstructure:"consul_token"`
	ConsulDC           string            `yaml:"consul_dc" mapstructure:"consul_dc"`
	ConsulNamespace    string            `yaml:"consul_namespace" mapstructure:"consul_namespace"`
	WriteRateLimit     string            `yaml:"write_rate_limit" mapstructure:"write_rate_limit"`
	DampeningPeriod    string            `yaml:"dampening_period" mapstructure:"dampening_period"`
	MinDampeningPeriod string            `yaml:"min_dampening_period" mapstructure:"min_dampening_period"`
	// ResyncInterval is how often the leader reconciles everything even without catalog
	// events, to retry failed writes and repair drift. Empty means the default (60s); "0s" disables it.
	ResyncInterval string           `yaml:"resync_interval" mapstructure:"resync_interval"`
	Strategies     StrategiesConfig `yaml:"strategies" json:"strategies" mapstructure:"strategies"`
	HA             HaConfig         `yaml:"ha" json:"ha" mapstructure:"ha"`
	Auth           AuthConfig       `yaml:"auth" json:"auth" mapstructure:"auth"`
	DryRun         bool             `yaml:"dry_run" json:"dry_run" mapstructure:"dry_run"`
}

type contextKey string

const (
	tokenContextKey    contextKey = "atc-token"
	clientIPContextKey contextKey = "atc-client-ip"
)

var coreTracer = otel.Tracer("atc/core")

// leaderReelectBackoff is how long a node waits before re-entering the election after
// voluntarily releasing leadership because its workload exited. Var for tests.
var leaderReelectBackoff = 5 * time.Second

type Atc struct {
	Cfg            Config
	cfgMu          sync.RWMutex
	logger         *slog.Logger
	coreLogger     *slog.Logger
	Server         *atc_server.Server
	enabledModules map[string]bool
	otelShutdown   func(context.Context) error

	Watcher    *watcher.ConsulWatcher
	Forwarder  *forwarder.Forwarder
	Redirector *redirector.Redirector

	consulClientsMu sync.RWMutex
	consulClients   map[string]*api.Client

	requestHTTPClientOnce sync.Once
	requestHTTPClient     *http.Client
	requestHTTPClientErr  error

	tokenCacheMu sync.RWMutex
	tokenCache   map[string]time.Time

	shutdownRequested atomic.Bool

	overridesCountMu sync.Mutex
	overridesCount   int64
	overridesCountAt time.Time

	consulStatusMu sync.Mutex
	consulStatus   ConsulStatus

	isLeader         atomic.Bool
	forwarderLeader  atomic.Bool
	redirectorLeader atomic.Bool

	leaderTransitionsCounter metric.Int64Counter
	overridesPurgedCounter   metric.Int64Counter
	configReloadsCounter     metric.Int64Counter
	configLastReloadTime     int64
}

func resolveNodeName(cfg Config) string {
	if cfg.NodeName != "" {
		return cfg.NodeName
	}
	hn, _ := os.Hostname()
	if cfg.Name == "" {
		if hn != "" {
			return "atc-node-" + hn
		}
		return "atc-node"
	}
	if cfg.Name == "atc-service" && hn != "" {
		return cfg.Name + "-" + hn
	}
	return cfg.Name
}

// NewCLI initializes a lightweight Atc instance for one-shot CLI operations (such as leader status or force-unlock)
// without starting OpenTelemetry exporters, HTTP servers, or catalog watchers.
func NewCLI(cfg Config) *Atc {
	cfg.Name = resolveNodeName(cfg)
	logger := initLogger(cfg.Server.LogFormat, cfg.Server.LogLevel)
	return &Atc{
		Cfg:            cfg,
		logger:         logger,
		coreLogger:     logger.With(slog.String("module", "core")),
		enabledModules: resolveModules(cfg.Target),
		consulClients:  make(map[string]*api.Client),
		tokenCache:     make(map[string]time.Time),
	}
}

func New(cfg Config) (*Atc, error) {
	cfg.Name = resolveNodeName(cfg)

	logger := initLogger(cfg.Server.LogFormat, cfg.Server.LogLevel)

	otelShutdown, err := telemetry.Init(context.Background(), "atc")
	if err != nil {
		logger.Warn("Failed to initialize OpenTelemetry SDK", slog.Any("error", err))
	}

	meter := otel.Meter("atc/core")
	leaderTransitionsCounter, _ := meter.Int64Counter(
		"atc_ha_leader_transitions_total",
		metric.WithDescription("Total number of leadership transitions on this node"),
	)
	overridesPurgedCounter, _ := meter.Int64Counter(
		"atc_overrides_purged_total",
		metric.WithDescription("Total number of expired manual overrides purged"),
	)
	configReloadsCounter, _ := meter.Int64Counter(
		"atc_config_reloads_total",
		metric.WithDescription("Total number of configuration reloads"),
	)

	atc := &Atc{
		Cfg:                      cfg,
		logger:                   logger,
		coreLogger:               logger.With(slog.String("module", "core")),
		enabledModules:           resolveModules(cfg.Target),
		otelShutdown:             otelShutdown,
		consulClients:            make(map[string]*api.Client),
		tokenCache:               make(map[string]time.Time),
		leaderTransitionsCounter: leaderTransitionsCounter,
		overridesPurgedCounter:   overridesPurgedCounter,
		configReloadsCounter:     configReloadsCounter,
		configLastReloadTime:     time.Now().Unix(),
	}

	if atc.enabledModules[Forwarder] || atc.enabledModules[Redirector] {
		atc.Watcher = watcher.New(
			logger.With(slog.String("module", "watcher")),
			cfg.ConsulAddr,
			cfg.ConsulToken,
			cfg.ConsulDC,
			cfg.ConsulNamespace,
		)
	}

	_, err = meter.Int64ObservableGauge(
		"atc_ha_is_leader",
		metric.WithDescription("Leadership status on this node (1 = Leader, 0 = Standby)"),
		metric.WithInt64Callback(func(_ context.Context, obsrv metric.Int64Observer) error {
			if atc.Forwarder != nil {
				val := int64(0)
				if atc.forwarderLeader.Load() {
					val = 1
				}
				obsrv.Observe(val, metric.WithAttributes(attribute.String("module", "forwarder")))
			}
			if atc.Redirector != nil {
				val := int64(0)
				if atc.redirectorLeader.Load() {
					val = 1
				}
				obsrv.Observe(val, metric.WithAttributes(attribute.String("module", "redirector")))
			}
			if atc.Forwarder == nil && atc.Redirector == nil {
				val := int64(0)
				if atc.isLeader.Load() {
					val = 1
				}
				obsrv.Observe(val, metric.WithAttributes(attribute.String("module", "core")))
			}
			return nil
		}),
	)
	if err != nil {
		logger.Warn("Failed to register atc_ha_is_leader gauge", slog.Any("error", err))
	}

	_, err = meter.Int64ObservableGauge(
		"atc_overrides_active",
		metric.WithDescription("Number of active manual overrides in Consul"),
		metric.WithInt64Callback(func(ctx context.Context, obsrv metric.Int64Observer) error {
			if n, ok := atc.activeOverridesCount(ctx); ok {
				obsrv.Observe(n)
			}
			return nil
		}),
	)
	if err != nil {
		logger.Warn("Failed to register atc_overrides_active gauge", slog.Any("error", err))
	}

	_, err = meter.Int64ObservableGauge(
		"atc_config_last_reload_timestamp_seconds",
		metric.WithDescription("Unix timestamp of the last configuration reload"),
		metric.WithInt64Callback(func(_ context.Context, obsrv metric.Int64Observer) error {
			obsrv.Observe(atomic.LoadInt64(&atc.configLastReloadTime))
			return nil
		}),
	)
	if err != nil {
		logger.Warn("Failed to register atc_config_last_reload_timestamp_seconds gauge", slog.Any("error", err))
	}

	_, err = meter.Int64ObservableGauge(
		"atc_strategies_configured",
		metric.WithDescription("Number of strategies configured in memory"),
		metric.WithInt64Callback(func(_ context.Context, obsrv metric.Int64Observer) error {
			atc.cfgMu.RLock()
			foCount := len(atc.Cfg.Strategies.Failover)
			rdCount := len(atc.Cfg.Strategies.Redirect)
			atc.cfgMu.RUnlock()
			obsrv.Observe(int64(foCount), metric.WithAttributes(attribute.String("strategy_type", "failover")))
			obsrv.Observe(int64(rdCount), metric.WithAttributes(attribute.String("strategy_type", "redirect")))
			return nil
		}),
	)
	if err != nil {
		logger.Warn("Failed to register atc_strategies_configured gauge", slog.Any("error", err))
	}

	if atc.enabledModules[Server] {
		if err := atc.initServer(); err != nil {
			return nil, err
		}
	}

	if atc.enabledModules[Forwarder] {
		if err := atc.initForwarder(); err != nil {
			return nil, err
		}
	}

	if atc.enabledModules[Redirector] {
		if err := atc.initRedirector(); err != nil {
			return nil, err
		}
	}

	if atc.Forwarder != nil && atc.Redirector != nil {
		atc.Forwarder.SetRedirector(atc.Redirector)
		atc.Redirector.SetForwarder(atc.Forwarder)
	}

	return atc, nil
}

func (t *Atc) Run(ctx context.Context) error {
	// The config file watcher is already running, so read config under the lock.
	t.cfgMu.RLock()
	targets := t.Cfg.Target
	haEnabled := t.Cfg.HA.Enabled
	t.cfgMu.RUnlock()

	for _, module := range targets {
		if !slices.Contains(UserVisibleModules, module) {
			return fmt.Errorf("selected target (%s) is an internal module or invalid target, which is not allowed", module)
		}
	}

	g, ctx := errgroup.WithContext(ctx)

	if t.otelShutdown != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := t.otelShutdown(shutdownCtx); err != nil {
				t.coreLogger.ErrorContext(ctx, "OpenTelemetry SDK shutdown failed", slog.Any("error", err))
			}
		}()
	}

	if t.Server != nil {
		t.Server.Mux.HandleFunc("/health", t.healthHandler)
		t.Server.Mux.HandleFunc("/ready", t.readyHandler)
		t.Server.Mux.Handle("GET /services", t.authMiddleware(http.HandlerFunc(t.servicesHandler)))
	}

	if t.Server != nil {
		g.Go(func() error {
			return t.Server.Run(ctx)
		})
	}

	if haEnabled {
		g.Go(func() error {
			return t.runLeaderElection(ctx)
		})
	} else {
		if t.Forwarder != nil {
			g.Go(func() error {
				return t.Forwarder.Run(ctx)
			})
		}
		if t.Redirector != nil {
			g.Go(func() error {
				return t.Redirector.Run(ctx)
			})
		}
	}

	t.coreLogger.InfoContext(ctx, "Application started")

	g.Go(func() error {
		t.sweepExpiredOverrides(ctx)
		return nil
	})

	go func() {
		<-ctx.Done()
		t.shutdownRequested.Store(true)
	}()

	err := g.Wait()
	if err != nil {
		t.coreLogger.ErrorContext(ctx, "Application stopped with error", slog.Any("err", err))
	} else {
		t.coreLogger.InfoContext(ctx, "Application stopped gracefully")
	}

	return err
}

// managedByOf describes who controls an existing resolver, for the dashboard.
func managedByOf(e *api.ServiceResolverConfigEntry, tags []string) string {
	switch {
	case e.Meta != nil && e.Meta["created-by"] == forwarder.CreatedByOverride:
		return "override"
	case forwarder.IsATCOwned(e):
		return "atc"
	case slices.Contains(tags, forwarder.AdoptTag):
		return "adopting"
	default:
		return "foreign"
	}
}

// ErrForeignResolver is returned when an override would replace a service-resolver that ATC
// did not create. Clearing the override later deletes the entry, so the original would be lost.
var ErrForeignResolver = errors.New("service already has a service-resolver that ATC did not create")

// checkOverrideAllowed refuses to replace a resolver ATC does not own unless force is set.
func checkOverrideAllowed(ctx context.Context, client *api.Client, service string, force bool) error {
	if force {
		return nil
	}
	entry, _, err := client.ConfigEntries().Get("service-resolver", service, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		var se api.StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("failed to check existing service-resolver: %w", err)
	}
	if res, ok := entry.(*api.ServiceResolverConfigEntry); ok && !forwarder.IsATCOwned(res) {
		return fmt.Errorf("%w: %q. Applying an override replaces it, and clearing the override later deletes it, so the original configuration would be lost. Retry with force to replace it anyway", ErrForeignResolver, service)
	}
	return nil
}

// requestReconcile asks running forwarder/redirector loops on this node to reconcile, for
// changes that produce no catalog event (config reloads, deleted resolvers). On a standby
// node nothing is subscribed; the leader's periodic resync picks the change up instead.
func (t *Atc) requestReconcile(reason string) {
	if t.Watcher != nil {
		t.Watcher.Events.Broadcast(reason)
	}
}

// resyncInterval returns the configured periodic resync interval.
func resyncInterval(cfg Config) time.Duration {
	if cfg.ResyncInterval == "" {
		return forwarder.DefaultResyncInterval
	}
	d, err := time.ParseDuration(cfg.ResyncInterval)
	if err != nil || d < 0 {
		return forwarder.DefaultResyncInterval
	}
	return d
}

func (t *Atc) IsLeader() bool {
	t.cfgMu.RLock()
	haEnabled := t.Cfg.HA.Enabled
	t.cfgMu.RUnlock()
	if !haEnabled {
		return true
	}
	if t.Forwarder == nil && t.Redirector == nil {
		return t.isLeader.Load()
	}

	return (t.Forwarder == nil || t.forwarderLeader.Load()) &&
		(t.Redirector == nil || t.redirectorLeader.Load())
}

// isSweepOwner reports whether this node should purge expired overrides. With target-scoped
// locks the forwarder and redirector leaders can live on different nodes, so IsLeader()
// (which requires both) may be false everywhere. Exactly one lock owner sweeps instead:
// the forwarder leader if a forwarder runs, else the redirector leader, else the core leader.
func (t *Atc) isSweepOwner() bool {
	t.cfgMu.RLock()
	haEnabled := t.Cfg.HA.Enabled
	t.cfgMu.RUnlock()
	if !haEnabled {
		return true
	}
	switch {
	case t.Forwarder != nil:
		return t.forwarderLeader.Load()
	case t.Redirector != nil:
		return t.redirectorLeader.Load()
	default:
		return t.isLeader.Load()
	}
}

func (t *Atc) runLeaderElection(ctx context.Context) error {
	t.cfgMu.RLock()
	haEnabled := t.Cfg.HA.Enabled
	t.cfgMu.RUnlock()
	if !haEnabled {
		return nil
	}

	if t.Forwarder == nil && t.Redirector == nil {
		return t.runModuleLeaderElection(ctx, "", nil, &t.isLeader)
	}

	g, ctx := errgroup.WithContext(ctx)

	if t.Forwarder != nil {
		g.Go(func() error {
			return t.runModuleLeaderElection(ctx, "forwarder", t.Forwarder.Run, &t.forwarderLeader)
		})
	}
	if t.Redirector != nil {
		g.Go(func() error {
			return t.runModuleLeaderElection(ctx, "redirector", t.Redirector.Run, &t.redirectorLeader)
		})
	}

	return g.Wait()
}

func (t *Atc) runModuleLeaderElection(ctx context.Context, moduleName string, runFunc func(context.Context) error, leaderFlag *atomic.Bool) error {
	client, err := t.getConsulClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create consul client for lock: %w", err)
	}

	t.cfgMu.RLock()
	lockKey := t.Cfg.HA.LockKey
	sessionTTL := t.Cfg.HA.SessionTTL
	name := t.Cfg.Name
	t.cfgMu.RUnlock()

	if lockKey == "" {
		lockKey = "atc/leader/lock"
	}
	if moduleName != "" {
		lockKey = lockKey + "/" + moduleName
	}
	if sessionTTL == "" {
		sessionTTL = "15s"
	}
	if name == "" {
		if hn, err := os.Hostname(); err == nil && hn != "" {
			name = "atc-node-" + hn
		} else {
			name = "atc-node"
		}
	}

	opts := &api.LockOptions{
		Key:         lockKey,
		Value:       []byte(name),
		SessionTTL:  sessionTTL,
		SessionName: "atc-leader-election",
	}

	t.coreLogger.InfoContext(ctx, "Starting leader election loop", slog.String("lock_key", lockKey), slog.String("module", moduleName))

	ctx, span := coreTracer.Start(ctx, "ha/LeadershipLock", trace.WithAttributes(
		attribute.String("module", moduleName),
		attribute.String("lock_key", lockKey),
	))
	defer span.End()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Re-resolve the client each attempt so a reloaded Consul address or token is used
		// the next time this node competes for leadership.
		if c, cerr := t.getConsulClient(ctx); cerr == nil {
			client = c
		}
		lock, err := client.LockOpts(opts)
		if err != nil {
			t.coreLogger.ErrorContext(ctx, "failed to configure consul lock, retrying in 5s", slog.Any("error", err), slog.String("lock_key", lockKey))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
				continue
			}
		}

		t.coreLogger.InfoContext(ctx, "Attempting to acquire leadership lock...", slog.String("lock_key", lockKey))
		lostCh, err := lock.Lock(ctx.Done())
		if err != nil {
			t.coreLogger.ErrorContext(ctx, "Error acquiring leadership lock, retrying in 5s", slog.Any("error", err), slog.String("lock_key", lockKey))
			span.RecordError(err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
				continue
			}
		}

		if lostCh == nil {
			continue
		}

		leaderFlag.Store(true)
		t.coreLogger.InfoContext(ctx, "Leadership acquired. Starting workload...", slog.String("lock_key", lockKey))
		if t.leaderTransitionsCounter != nil {
			t.leaderTransitionsCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("module", moduleName),
				attribute.String("transition", "acquired"),
			))
		}
		span.AddEvent("leadership_acquired")

		leaderCtx, cancelLeader := context.WithCancel(ctx)
		leaderCtx, activeSpan := coreTracer.Start(leaderCtx, "ha/LeaderActive", trace.WithAttributes(
			attribute.String("module", moduleName),
			attribute.String("lock_key", lockKey),
		))

		// workloadDone is closed when the workload returns. It stays nil (never fires)
		// when there is no workload to run.
		var workloadDone chan struct{}
		if runFunc != nil {
			workloadDone = make(chan struct{})
			go func() {
				defer close(workloadDone)
				if err := runFunc(leaderCtx); err != nil {
					t.coreLogger.ErrorContext(leaderCtx, "workload run failed", slog.Any("error", err), slog.String("lock_key", lockKey))
					activeSpan.RecordError(err)
				}
			}()
		}

		stepDown := func(transition string) {
			activeSpan.AddEvent("leadership_" + transition)
			activeSpan.End()
			span.AddEvent("leadership_" + transition)
			if t.leaderTransitionsCounter != nil {
				t.leaderTransitionsCounter.Add(ctx, 1, metric.WithAttributes(
					attribute.String("module", moduleName),
					attribute.String("transition", transition),
				))
			}
			// Flip the leader flag before cancelling so in-flight writes observe the step-down.
			leaderFlag.Store(false)
			cancelLeader()
			if workloadDone != nil {
				<-workloadDone
			}
		}

		select {
		case <-ctx.Done():
			t.coreLogger.InfoContext(ctx, "Releasing leadership lock in Consul on graceful shutdown...", slog.String("lock_key", lockKey))
			leaderFlag.Store(false)
			cancelLeader()
			if workloadDone != nil {
				<-workloadDone
			}
			activeSpan.End()
			_ = lock.Unlock()
			return nil
		case <-lostCh:
			t.coreLogger.WarnContext(ctx, "Leadership lock lost. Stopping workload and reverting to standby...", slog.String("lock_key", lockKey))
			stepDown("lost")
		case <-workloadDone:
			// The workload exited while we still hold the lock. Holding on would leave the
			// cluster with a leader that does nothing, so release the lock and let another
			// node (or this one, after a backoff) take over.
			t.coreLogger.WarnContext(ctx, "Workload exited while holding leadership. Releasing lock and re-entering election...", slog.String("lock_key", lockKey))
			stepDown("released")
			if err := lock.Unlock(); err != nil {
				t.coreLogger.ErrorContext(ctx, "Failed to release leadership lock after workload exit", slog.Any("error", err), slog.String("lock_key", lockKey))
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(leaderReelectBackoff):
			}
		}
	}
}

// healthHandler is the liveness probe. It deliberately does not depend on Consul: a Consul
// outage must not make the orchestrator restart every ATC replica.
func (t *Atc) healthHandler(w http.ResponseWriter, _ *http.Request) {
	if t.shutdownRequested.Load() {
		t.coreLogger.Debug("application is stopping")
		http.Error(w, "Application is stopping", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, "OK")
}

// readyHandler is the readiness probe. It only fails while shutting down. It deliberately
// does NOT depend on Consul: Consul is shared by all replicas, so failing readiness on a
// Consul outage would pull every replica out of the Service at once and leave operators
// with a bare load-balancer error instead of a dashboard that explains the outage.
// Consul health is reported by GET /api/status and shown in the dashboard instead.
func (t *Atc) readyHandler(w http.ResponseWriter, r *http.Request) {
	if err := t.CheckReadiness(r.Context()); err != nil {
		http.Error(w, "Not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, "OK")
}

// CheckReadiness reports why this instance cannot serve requests, or nil if it can.
func (t *Atc) CheckReadiness(_ context.Context) error {
	if t.shutdownRequested.Load() {
		return errors.New("application is stopping")
	}
	return nil
}

// overridesGaugeTTL bounds how often the atc_overrides_active gauge queries Consul. Without it
// every metrics scrape (and OTLP export) on every replica listed all service-resolvers.
var overridesGaugeTTL = 30 * time.Second

// activeOverridesCount returns the number of active manual overrides, cached for
// overridesGaugeTTL. ok is false when no value is available yet and Consul cannot be queried.
func (t *Atc) activeOverridesCount(ctx context.Context) (n int64, ok bool) {
	t.overridesCountMu.Lock()
	defer t.overridesCountMu.Unlock()
	if !t.overridesCountAt.IsZero() && time.Since(t.overridesCountAt) < overridesGaugeTTL {
		return t.overridesCount, true
	}
	overrides, err := t.ListActiveOverrides(ctx)
	if err != nil {
		// Keep reporting the last known value rather than dropping the series.
		return t.overridesCount, !t.overridesCountAt.IsZero()
	}
	t.overridesCount = int64(len(overrides))
	t.overridesCountAt = time.Now()
	return t.overridesCount, true
}

// consulStatusCacheTTL bounds how often status checks hit Consul. Var for tests.
var consulStatusCacheTTL = 5 * time.Second

// ConsulStatus is what the dashboard shows about ATC's connection to Consul. It is served
// without authentication, so it carries fixed, human-readable messages only; raw errors
// (which may contain internal addresses) are logged instead.
type ConsulStatus struct {
	Reachable   bool       `json:"reachable"`
	Problem     string     `json:"problem,omitempty"` // "unreachable" or "no_leader"
	Message     string     `json:"message"`
	LastContact *time.Time `json:"last_contact,omitempty"`
	CheckedAt   time.Time  `json:"checked_at"`
}

const (
	consulProblemUnreachable = "unreachable"
	consulProblemNoLeader    = "no_leader"
)

// GetConsulStatus checks Consul with ATC's own token, cached for consulStatusCacheTTL.
// Changes in reachability are logged once, with the underlying error.
func (t *Atc) GetConsulStatus(ctx context.Context) ConsulStatus {
	t.consulStatusMu.Lock()
	defer t.consulStatusMu.Unlock()
	if !t.consulStatus.CheckedAt.IsZero() && time.Since(t.consulStatus.CheckedAt) < consulStatusCacheTTL {
		return t.consulStatus
	}

	prev := t.consulStatus
	st := ConsulStatus{CheckedAt: time.Now(), LastContact: prev.LastContact}
	err := t.probeConsul(ctx)
	switch {
	case err == nil:
		now := st.CheckedAt
		st.Reachable, st.LastContact, st.Message = true, &now, "Connected to Consul."
	case errors.Is(err, errConsulNoLeader):
		st.Problem, st.Message = consulProblemNoLeader, "Consul is reachable but has no cluster leader, so it cannot serve or store routing configuration."
	default:
		st.Problem, st.Message = consulProblemUnreachable, "ATC cannot reach Consul."
	}

	firstCheck := prev.CheckedAt.IsZero()
	if st.Reachable && !firstCheck && !prev.Reachable {
		t.coreLogger.InfoContext(ctx, "Consul connection restored")
	} else if !st.Reachable && (firstCheck || prev.Reachable || prev.Problem != st.Problem) {
		t.coreLogger.WarnContext(ctx, "Consul is not usable", slog.String("problem", st.Problem), slog.Any("error", err))
	}

	t.consulStatus = st
	return st
}

var errConsulNoLeader = errors.New("consul cluster has no leader")

func (t *Atc) apiStatusHandler(w http.ResponseWriter, r *http.Request) {
	st := t.GetConsulStatus(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"consul": st})
}

// ConsulStatusMessage lets the MCP check_readiness tool describe Consul connectivity.
func (t *Atc) ConsulStatusMessage(ctx context.Context) (bool, string) {
	st := t.GetConsulStatus(ctx)
	return st.Reachable, st.Message
}

func (t *Atc) probeConsul(ctx context.Context) error {
	t.cfgMu.RLock()
	addr, token, dc, ns := t.Cfg.ConsulAddr, t.Cfg.ConsulToken, t.Cfg.ConsulDC, t.Cfg.ConsulNamespace
	t.cfgMu.RUnlock()

	client, err := t.getOrCreateConsulClient(addr, token, dc, ns)
	if err != nil {
		return fmt.Errorf("consul client: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	leader, err := client.Status().LeaderWithQueryOptions((&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return fmt.Errorf("consul unreachable: %w", err)
	}
	if leader == "" {
		return errConsulNoLeader
	}
	return nil
}

func (t *Atc) GetEnabledModulesTable() string {
	return RenderServicesTable(t.enabledModules)
}

func (t *Atc) GetAtcEnabledServices(ctx context.Context) ([]string, error) {
	client, err := t.getConsulClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to consul: %w", err)
	}

	var services map[string][]string
	err = t.traceConsulCall(ctx, "list_services", func() error {
		var err error
		services, _, err = client.Catalog().Services((&api.QueryOptions{}).WithContext(ctx))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch consul services: %w", err)
	}

	var enabled []string
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			enabled = append(enabled, svcName)
		}
	}
	slices.Sort(enabled)
	return enabled, nil
}

func (t *Atc) apiServicesHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t.cfgMu.RLock()
	consulNamespace := t.Cfg.ConsulNamespace
	t.cfgMu.RUnlock()
	client, err := t.getConsulClient(ctx)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to connect to consul: %v", err))
		return
	}

	var services map[string][]string
	err = t.traceConsulCall(ctx, "list_services", func() error {
		var err error
		services, _, err = client.Catalog().Services((&api.QueryOptions{}).WithContext(ctx))
		return err
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to fetch services: %v", err))
		return
	}

	// Fetch all service-resolver config entries to find deleted ones
	var entries []api.ConfigEntry
	err = t.traceConsulCall(ctx, "list_config_entries", func() error {
		var err error
		entries, _, err = client.ConfigEntries().List("service-resolver", (&api.QueryOptions{}).WithContext(ctx))
		return err
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list config entries: %v", err))
		return
	}

	existingResolverEntries := make(map[string]*api.ServiceResolverConfigEntry)
	for _, entry := range entries {
		if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
			existingResolverEntries[res.Name] = res
		}
	}

	type TargetItem struct {
		Service    string `json:"service"`
		Datacenter string `json:"datacenter"`
	}

	type ServiceItem struct {
		Name             string            `json:"name"`
		Namespace        string            `json:"namespace,omitempty"`
		Tags             []string          `json:"tags"`
		ResolverType     string            `json:"resolver_type"`
		Status           string            `json:"status"` // "active" or "deleted"
		FailoverStrategy string            `json:"failover_strategy"`
		RedirectStrategy string            `json:"redirect_strategy"`
		FailoverTargets  []TargetItem      `json:"failover_targets,omitempty"`
		RedirectTarget   *TargetItem       `json:"redirect_target,omitempty"`
		Meta             map[string]string `json:"meta,omitempty"`
		// ManagedBy is "atc", "override", "foreign" (an existing resolver ATC leaves alone),
		// "adopting" (foreign, but tagged atc.adopt=true) or "none" (no resolver yet).
		ManagedBy string `json:"managed_by"`
	}

	resultMap := make(map[string]ServiceItem)

	// 1. Process active catalog services
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			resType := "none"
			failoverStrategy := getStrategyFromTags(tags, "atc.failover=")
			redirectStrategy := getStrategyFromTags(tags, "atc.redirect=")
			var failoverTargets []TargetItem
			var redirectTarget *TargetItem

			var entryMeta map[string]string
			managedBy := "none"
			if existing, exists := existingResolverEntries[svcName]; exists {
				entryMeta = existing.Meta
				managedBy = managedByOf(existing, tags)
				if len(existing.Failover) > 0 {
					resType = "failover"
				} else if existing.Redirect != nil && existing.Redirect.Service != "" {
					resType = "redirect"
				}
				if existing.Meta != nil {
					if failoverStrategy == "" {
						failoverStrategy = existing.Meta["failover-strategy"]
					}
					if redirectStrategy == "" {
						redirectStrategy = existing.Meta["redirect-strategy"]
					}
				}
				if existing.Failover != nil {
					if fo, ok := existing.Failover["*"]; ok {
						for _, target := range fo.Targets {
							failoverTargets = append(failoverTargets, TargetItem{
								Service:    target.Service,
								Datacenter: target.Datacenter,
							})
						}
					}
				}
				if existing.Redirect != nil {
					redirectTarget = &TargetItem{
						Service:    existing.Redirect.Service,
						Datacenter: existing.Redirect.Datacenter,
					}
				}
			} else {
				// Fallback if config entry does not exist yet
				if t.enabledModules[Forwarder] {
					resType = "failover"
				} else if t.enabledModules[Redirector] {
					resType = "none"
				}
			}

			resultMap[svcName] = ServiceItem{
				Name:             svcName,
				Namespace:        consulNamespace,
				Tags:             tags,
				ResolverType:     resType,
				Status:           "active",
				FailoverStrategy: failoverStrategy,
				RedirectStrategy: redirectStrategy,
				FailoverTargets:  failoverTargets,
				RedirectTarget:   redirectTarget,
				Meta:             entryMeta,
				ManagedBy:        managedBy,
			}
		}
	}

	// 2. Process config entries to find deleted services that are being redirected
	for _, entry := range entries {
		if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
			if res.Meta != nil && (res.Meta["created-by"] == "atc" || res.Meta["created-by"] == "atc-override") {
				if _, active := resultMap[res.Name]; !active {
					// Check if completely absent from catalog
					if _, exists := services[res.Name]; !exists {
						resType := "redirect"
						if len(res.Failover) > 0 {
							resType = "failover"
						}
						failoverStrategy := res.Meta["failover-strategy"]
						if failoverStrategy == "" {
							if t.Forwarder != nil {
								failoverStrategy, _ = t.Forwarder.GetCachedStrategies(res.Name)
							}
							if failoverStrategy == "" && t.Redirector != nil {
								failoverStrategy, _ = t.Redirector.GetCachedStrategies(res.Name)
							}
						}
						redirectStrategy := res.Meta["redirect-strategy"]
						if redirectStrategy == "" {
							if t.Redirector != nil {
								_, redirectStrategy = t.Redirector.GetCachedStrategies(res.Name)
							}
							if redirectStrategy == "" && t.Forwarder != nil {
								_, redirectStrategy = t.Forwarder.GetCachedStrategies(res.Name)
							}
						}
						var failoverTargets []TargetItem
						var redirectTarget *TargetItem

						if res.Failover != nil {
							if fo, ok := res.Failover["*"]; ok {
								for _, target := range fo.Targets {
									failoverTargets = append(failoverTargets, TargetItem{
										Service:    target.Service,
										Datacenter: target.Datacenter,
									})
								}
							}
						}
						if res.Redirect != nil {
							redirectTarget = &TargetItem{
								Service:    res.Redirect.Service,
								Datacenter: res.Redirect.Datacenter,
							}
						}

						ns := res.Namespace
						if ns == "" {
							ns = consulNamespace
						}

						resultMap[res.Name] = ServiceItem{
							Name:             res.Name,
							Namespace:        ns,
							Tags:             []string{"atc.enabled=true", "status:deleted"},
							ResolverType:     resType,
							Status:           "deleted",
							FailoverStrategy: failoverStrategy,
							RedirectStrategy: redirectStrategy,
							FailoverTargets:  failoverTargets,
							RedirectTarget:   redirectTarget,
							Meta:             res.Meta,
							ManagedBy:        managedByOf(res, nil),
						}
					}
				}
			}
		}
	}

	result := make([]ServiceItem, 0, len(resultMap))
	for _, item := range resultMap {
		result = append(result, item)
	}

	slices.SortFunc(result, func(a, b ServiceItem) int {
		return strings.Compare(a.Name, b.Name)
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}

func (t *Atc) PurgeServiceResolver(ctx context.Context, name string) error {
	ctx, span := coreTracer.Start(ctx, "PurgeServiceResolver", trace.WithAttributes(attribute.String("service", name)))
	defer span.End()

	client, err := t.getConsulClient(ctx)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("failed to connect to consul: %w", err)
	}

	var entry api.ConfigEntry
	err = t.traceConsulCall(ctx, "get_config_entry", func() error {
		var err error
		entry, _, err = client.ConfigEntries().Get("service-resolver", name, nil)
		return err
	})
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("failed to fetch config entry: %w", err)
	}

	resolver, ok := entry.(*api.ServiceResolverConfigEntry)
	if !ok || resolver.Meta == nil || (resolver.Meta["created-by"] != "atc" && resolver.Meta["created-by"] != "atc-override") {
		err = fmt.Errorf("entry was not created by ATC")
		span.RecordError(err)
		return err
	}

	t.cfgMu.RLock()
	dryRun := t.Cfg.DryRun
	t.cfgMu.RUnlock()

	if dryRun {
		span.AddEvent("dry_run_purge_service_resolver", trace.WithAttributes(attribute.String("service", name)))
		t.coreLogger.InfoContext(ctx, "[DRY RUN] Would delete service-resolver config entry", slog.String("service", name))
	} else {
		span.AddEvent("purge_service_resolver", trace.WithAttributes(attribute.String("service", name)))
		err = t.traceConsulCall(ctx, "delete_config_entry", func() error {
			_, err := client.ConfigEntries().Delete("service-resolver", name, (&api.WriteOptions{}).WithContext(ctx))
			return err
		})
		if err != nil {
			span.RecordError(err)
			return fmt.Errorf("failed to delete config entry: %w", err)
		}
		// Deleting a resolver changes no catalog data, so without this the service would have
		// no resolver until an unrelated catalog event (e.g. after clearing an override).
		t.requestReconcile("resolver_purged")
	}

	return nil
}

func (t *Atc) ApplyFailoverOverride(ctx context.Context, service string, targetDc string, targetNs string, duration string, force bool) error {
	client, err := t.getConsulClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to consul: %w", err)
	}
	if err := checkOverrideAllowed(ctx, client, service, force); err != nil {
		return err
	}

	meta := map[string]string{
		"created-by": "atc-override",
	}
	if duration != "" {
		d, err := time.ParseDuration(duration)
		if err != nil {
			return fmt.Errorf("invalid duration format %q: %w", duration, err)
		}
		meta["atc-override-expires-at"] = time.Now().Add(d).Format(time.RFC3339)
	}

	entry := &api.ServiceResolverConfigEntry{
		Kind: "service-resolver",
		Name: service,
		Meta: meta,
		Failover: map[string]api.ServiceResolverFailover{
			"*": {
				Targets: []api.ServiceResolverFailoverTarget{
					{
						Service:    service,
						Datacenter: targetDc,
						Namespace:  targetNs,
					},
				},
			},
		},
	}

	ctx, span := coreTracer.Start(ctx, "ApplyFailoverOverride")
	defer span.End()

	t.cfgMu.RLock()
	dryRun := t.Cfg.DryRun
	t.cfgMu.RUnlock()

	if dryRun {
		span.AddEvent("dry_run_apply_failover_override", trace.WithAttributes(attribute.String("service", service), attribute.String("target_dc", targetDc), attribute.String("target_ns", targetNs)))
		t.coreLogger.InfoContext(ctx, "[DRY RUN] Would apply manual failover override", slog.String("service", service), slog.String("target_dc", targetDc), slog.String("target_ns", targetNs), slog.String("duration", duration))
	} else {
		span.AddEvent("apply_failover_override", trace.WithAttributes(attribute.String("service", service), attribute.String("target_dc", targetDc), attribute.String("target_ns", targetNs)))
		err = t.traceConsulCall(ctx, "set_config_entry", func() error {
			_, _, err := client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
			return err
		})
		if err != nil {
			return fmt.Errorf("failed to set failover override: %w", err)
		}
		t.coreLogger.InfoContext(ctx, "Applied manual failover override", slog.String("service", service), slog.String("target_dc", targetDc), slog.String("target_ns", targetNs))
	}

	return nil
}

func (t *Atc) TriggerManualRedirect(ctx context.Context, service string, redirectDc string, redirectNs string, duration string, force bool) error {
	client, err := t.getConsulClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to consul: %w", err)
	}
	if err := checkOverrideAllowed(ctx, client, service, force); err != nil {
		return err
	}

	meta := map[string]string{
		"created-by": "atc-override",
	}
	if duration != "" {
		d, err := time.ParseDuration(duration)
		if err != nil {
			return fmt.Errorf("invalid duration format %q: %w", duration, err)
		}
		meta["atc-override-expires-at"] = time.Now().Add(d).Format(time.RFC3339)
	}

	entry := &api.ServiceResolverConfigEntry{
		Kind: "service-resolver",
		Name: service,
		Meta: meta,
		Redirect: &api.ServiceResolverRedirect{
			Service:    service,
			Datacenter: redirectDc,
			Namespace:  redirectNs,
		},
	}

	ctx, span := coreTracer.Start(ctx, "TriggerManualRedirect")
	defer span.End()

	t.cfgMu.RLock()
	dryRun := t.Cfg.DryRun
	t.cfgMu.RUnlock()

	if dryRun {
		span.AddEvent("dry_run_trigger_manual_redirect", trace.WithAttributes(attribute.String("service", service), attribute.String("redirect_dc", redirectDc), attribute.String("redirect_ns", redirectNs)))
		t.coreLogger.InfoContext(ctx, "[DRY RUN] Would apply manual redirect override", slog.String("service", service), slog.String("redirect_dc", redirectDc), slog.String("redirect_ns", redirectNs), slog.String("duration", duration))
	} else {
		span.AddEvent("trigger_manual_redirect", trace.WithAttributes(attribute.String("service", service), attribute.String("redirect_dc", redirectDc), attribute.String("redirect_ns", redirectNs)))
		err = t.traceConsulCall(ctx, "set_config_entry", func() error {
			_, _, err := client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
			return err
		})
		if err != nil {
			return fmt.Errorf("failed to set redirect override: %w", err)
		}
		t.coreLogger.InfoContext(ctx, "Applied manual redirect override", slog.String("service", service), slog.String("redirect_dc", redirectDc), slog.String("redirect_ns", redirectNs))
	}

	return nil
}

func (t *Atc) apiServicesDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	svcName := r.URL.Query().Get("name")
	if svcName == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing 'name' query parameter")
		return
	}

	err := t.PurgeServiceResolver(r.Context(), svcName)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "404") {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if strings.Contains(err.Error(), "not created by ATC") {
			writeJSONError(w, http.StatusForbidden, err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	t.logAudit(r.Context(), r, "purge_resolver", svcName, nil)
	w.WriteHeader(http.StatusNoContent)
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

func (t *Atc) getOrCreateConsulClient(addr, token, dc, ns string) (*api.Client, error) {
	key := addr + "|" + token + "|" + dc + "|" + ns

	t.consulClientsMu.RLock()
	if t.consulClients != nil {
		if client, ok := t.consulClients[key]; ok {
			t.consulClientsMu.RUnlock()
			return client, nil
		}
	}
	t.consulClientsMu.RUnlock()

	client, err := api.NewClient(consulCfg(addr, token, dc, ns))
	if err != nil {
		return nil, err
	}

	t.consulClientsMu.Lock()
	if t.consulClients == nil {
		t.consulClients = make(map[string]*api.Client)
	}
	t.consulClients[key] = client
	t.consulClientsMu.Unlock()
	return client, nil
}

// newRequestConsulClient builds a Consul client for a per-request token. Clients share one
// pooled HTTP client so creating them is cheap and does not leak connections or goroutines.
func (t *Atc) newRequestConsulClient(addr, token, dc, ns string) (*api.Client, error) {
	t.requestHTTPClientOnce.Do(func() {
		base := api.DefaultConfig()
		t.requestHTTPClient, t.requestHTTPClientErr = api.NewHttpClient(base.Transport, base.TLSConfig)
	})
	if t.requestHTTPClientErr != nil {
		return nil, t.requestHTTPClientErr
	}
	cfg := consulCfg(addr, token, dc, ns)
	cfg.HttpClient = t.requestHTTPClient
	return api.NewClient(cfg)
}

func (t *Atc) getConsulClient(ctx context.Context) (*api.Client, error) {
	t.cfgMu.RLock()
	addr := t.Cfg.ConsulAddr
	token := t.Cfg.ConsulToken
	dc := t.Cfg.ConsulDC
	ns := t.Cfg.ConsulNamespace
	t.cfgMu.RUnlock()

	if ctxToken, ok := ctx.Value(tokenContextKey).(string); ok && ctxToken != "" {
		t.cfgMu.RLock()
		static := isStaticKey(t.Cfg.Auth.StaticKeys, ctxToken)
		t.cfgMu.RUnlock()
		if !static {
			// Delegated Consul token: act as the caller so Consul's ACLs decide what they may
			// read or write. Not cached, so the client cache stays bounded by configuration.
			return t.newRequestConsulClient(addr, ctxToken, dc, ns)
		}
	}

	return t.getOrCreateConsulClient(addr, token, dc, ns)
}

func (t *Atc) GetPredefinedStrategies(ctx context.Context) (string, error) {
	t.cfgMu.RLock()
	s := t.Cfg.Strategies
	t.cfgMu.RUnlock()

	var sb strings.Builder
	if len(s.Failover) == 0 && len(s.Redirect) == 0 {
		return "No predefined strategies configured.", nil
	}

	sb.WriteString("Predefined Strategies:\n")
	if len(s.Failover) > 0 {
		sb.WriteString("\nFailover Strategies:\n")
		for name, val := range s.Failover {
			fmt.Fprintf(&sb, "- %s: timeout=%s, targets:\n", name, val.ConnectTimeout)
			for _, target := range val.Targets {
				fmt.Fprintf(&sb, "  - Service: %s, Datacenter: %s\n", target.Service, target.Datacenter)
			}
		}
	}
	if len(s.Redirect) > 0 {
		sb.WriteString("\nRedirect Strategies:\n")
		for name, val := range s.Redirect {
			fmt.Fprintf(&sb, "- %s: Target Service: %s, Target Datacenter: %s\n", name, val.Service, val.Datacenter)
		}
	}
	return sb.String(), nil
}

func (t *Atc) GetFederationStatus(ctx context.Context) (map[string]string, error) {
	client, err := t.getConsulClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to consul: %w", err)
	}

	members, err := client.Agent().Members(true)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch consul WAN members: %w", err)
	}

	dcMap := make(map[string]string)
	for _, m := range members {
		dc := m.Tags["dc"]
		if dc == "" {
			continue
		}
		var statusStr string
		switch m.Status {
		case 1:
			statusStr = "alive"
		case 4:
			statusStr = "failed"
		default:
			statusStr = "offline"
		}
		if dcMap[dc] != "alive" {
			dcMap[dc] = statusStr
		}
	}
	return dcMap, nil
}

func (t *Atc) ReloadConfig(cfg Config) {
	cfg.Name = resolveNodeName(cfg)

	t.cfgMu.Lock()
	t.Cfg = cfg
	t.cfgMu.Unlock()

	if t.Watcher != nil {
		t.Watcher.SetConnection(cfg.ConsulAddr, cfg.ConsulToken, cfg.ConsulDC, cfg.ConsulNamespace)
	}
	if t.Forwarder != nil {
		t.Forwarder.SetConnection(cfg.ConsulAddr, cfg.ConsulToken, cfg.ConsulDC, cfg.ConsulNamespace)
		t.Forwarder.SetResyncInterval(resyncInterval(cfg))
	}
	if t.Redirector != nil {
		t.Redirector.SetConnection(cfg.ConsulAddr, cfg.ConsulToken, cfg.ConsulDC, cfg.ConsulNamespace)
		t.Redirector.SetResyncInterval(resyncInterval(cfg))
	}

	ctx, span := coreTracer.Start(context.Background(), "ReloadConfig")
	defer span.End()
	span.AddEvent("config_reloaded", trace.WithAttributes(attribute.Bool("dry_run", cfg.DryRun)))

	t.coreLogger.InfoContext(ctx, "Configuration reloaded dynamically from file watcher", slog.Bool("dry_run", cfg.DryRun))
	atomic.StoreInt64(&t.configLastReloadTime, time.Now().Unix())
	if t.configReloadsCounter != nil {
		t.configReloadsCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("status", "success"),
		))
	}

	if t.Forwarder != nil {
		t.Forwarder.UpdateConfig(
			cfg.Strategies.Failover,
			cfg.DampeningPeriod,
			cfg.MinDampeningPeriod,
			cfg.ConsulNamespace,
			cfg.WriteRateLimit,
			cfg.DryRun,
		)
	}
	if t.Redirector != nil {
		_, hasDefaultFailover := cfg.Strategies.Failover["default"]
		t.Redirector.SetHasDefaultFailover(hasDefaultFailover)
		t.Redirector.UpdateConfig(
			cfg.Strategies.Redirect,
			cfg.DampeningPeriod,
			cfg.MinDampeningPeriod,
			cfg.ConsulNamespace,
			cfg.WriteRateLimit,
			cfg.DryRun,
		)
	}

	// Apply the new strategies and settings now rather than on the next catalog change.
	t.requestReconcile("config_reload")
}

func (t *Atc) TriggerConfigReload() error {
	t.coreLogger.Info("Triggering manual configuration reload")
	if configFile := viper.GetString("config"); configFile != "" {
		if err := viper.ReadInConfig(); err != nil {
			if t.configReloadsCounter != nil {
				t.configReloadsCounter.Add(context.Background(), 1, metric.WithAttributes(
					attribute.String("status", "failure"),
				))
			}
			return fmt.Errorf("failed to read config file %s: %w", configFile, err)
		}
	}

	newCfg, err := LoadConfigFromViper(viper.GetViper())
	if err != nil {
		if t.configReloadsCounter != nil {
			t.configReloadsCounter.Add(context.Background(), 1, metric.WithAttributes(
				attribute.String("status", "failure"),
			))
		}
		return err
	}

	if errs := ValidateConfig(newCfg); len(errs) > 0 {
		if t.configReloadsCounter != nil {
			t.configReloadsCounter.Add(context.Background(), 1, metric.WithAttributes(
				attribute.String("status", "failure"),
			))
		}
		return fmt.Errorf("invalid configuration on reload: %v", errs[0])
	}

	t.ReloadConfig(newCfg)
	return nil
}

func (t *Atc) ListActiveOverrides(ctx context.Context) ([]map[string]any, error) {
	client, err := t.getConsulClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to consul: %w", err)
	}

	var entries []api.ConfigEntry
	err = t.traceConsulCall(ctx, "list_config_entries", func() error {
		var err error
		entries, _, err = client.ConfigEntries().List("service-resolver", (&api.QueryOptions{}).WithContext(ctx))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list config entries: %w", err)
	}

	var overrides []map[string]any
	for _, entry := range entries {
		res, ok := entry.(*api.ServiceResolverConfigEntry)
		if !ok || res.Meta == nil || res.Meta["created-by"] != "atc-override" {
			continue
		}

		typ := "none"
		targetDc := ""
		if len(res.Failover) > 0 {
			typ = "failover"
			if fo, ok := res.Failover["*"]; ok && len(fo.Targets) > 0 {
				targetDc = fo.Targets[0].Datacenter
			}
		} else if res.Redirect != nil && res.Redirect.Service != "" {
			typ = "redirect"
			targetDc = res.Redirect.Datacenter
		}

		expiresAt := res.Meta["atc-override-expires-at"]
		if expiresAt == "" {
			expiresAt = "never"
		}

		overrides = append(overrides, map[string]any{
			"service":    res.Name,
			"type":       typ,
			"target_dc":  targetDc,
			"expires_at": expiresAt,
		})
	}
	return overrides, nil
}

func (t *Atc) sweepExpiredOverrides(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	t.coreLogger.InfoContext(ctx, "Starting override expiration sweep loop")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !t.isSweepOwner() {
				continue
			}
			overrides, err := t.ListActiveOverrides(ctx)
			if err != nil {
				t.coreLogger.ErrorContext(ctx, "Failed to list active overrides during sweep", slog.Any("error", err))
				continue
			}

			now := time.Now()
			for _, o := range overrides {
				svcName, _ := o["service"].(string)
				expiresAtStr, _ := o["expires_at"].(string)
				if expiresAtStr == "" || expiresAtStr == "never" {
					continue
				}

				exp, err := time.Parse(time.RFC3339, expiresAtStr)
				if err != nil {
					t.coreLogger.ErrorContext(ctx, "Failed to parse override expiration time", slog.String("service", svcName), slog.String("expires_at", expiresAtStr), slog.Any("error", err))
					continue
				}

				if now.After(exp) {
					t.coreLogger.InfoContext(ctx, "Override expired, purging", slog.String("service", svcName), slog.Time("expires_at", exp))

					purgeCtx, span := coreTracer.Start(ctx, "PurgeExpiredOverride", trace.WithAttributes(attribute.String("service", svcName)))

					t.cfgMu.RLock()
					dryRun := t.Cfg.DryRun
					t.cfgMu.RUnlock()

					if dryRun {
						span.AddEvent("dry_run_purge_expired_override", trace.WithAttributes(attribute.String("service", svcName)))
						t.coreLogger.InfoContext(purgeCtx, "[DRY RUN] Would purge expired service-resolver override", slog.String("service", svcName))
					} else {
						span.AddEvent("purge_expired_override", trace.WithAttributes(attribute.String("service", svcName)))
						err = t.PurgeServiceResolver(purgeCtx, svcName)
						if err != nil {
							t.coreLogger.ErrorContext(purgeCtx, "Failed to purge expired override", slog.String("service", svcName), slog.Any("error", err))
							span.RecordError(err)
						} else {
							t.coreLogger.InfoContext(purgeCtx, "Purged expired service-resolver override", slog.String("service", svcName))
							if t.overridesPurgedCounter != nil {
								t.overridesPurgedCounter.Add(purgeCtx, 1)
							}
						}
					}
					span.End()
				}
			}
		}
	}
}

func (t *Atc) traceConsulCall(ctx context.Context, op string, fn func() error) error {
	_, span := coreTracer.Start(ctx, "consul/"+op, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	err := fn()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}
