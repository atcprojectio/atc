package atc

import (
	"context"
	"encoding/json"
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
	"github.com/hashicorp/consul/api"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
}

type Config struct {
	Name               string            `yaml:"service" mapstructure:"service"`
	Server             atc_server.Config `yaml:"server" mapstructure:"server"`
	Target             []string          `yaml:"target" mapstructure:"target"`
	ConsulAddr         string            `yaml:"consul_addr" mapstructure:"consul_addr"`
	ConsulToken        string            `yaml:"consul_token" mapstructure:"consul_token"`
	ConsulDC           string            `yaml:"consul_dc" mapstructure:"consul_dc"`
	ConsulNamespace    string            `yaml:"consul_namespace" mapstructure:"consul_namespace"`
	WriteRateLimit     string            `yaml:"write_rate_limit" mapstructure:"write_rate_limit"`
	DampeningPeriod    string            `yaml:"dampening_period" mapstructure:"dampening_period"`
	MinDampeningPeriod string            `yaml:"min_dampening_period" mapstructure:"min_dampening_period"`
	Strategies         StrategiesConfig  `yaml:"strategies" json:"strategies" mapstructure:"strategies"`
	HA                 HaConfig          `yaml:"ha" json:"ha" mapstructure:"ha"`
	Auth               AuthConfig        `yaml:"auth" json:"auth" mapstructure:"auth"`
	DryRun             bool              `yaml:"dry_run" json:"dry_run" mapstructure:"dry_run"`
}

type contextKey string

const tokenContextKey contextKey = "atc-token"

var coreTracer = otel.Tracer("atc/core")

type Atc struct {
	Cfg            Config
	cfgMu          sync.RWMutex
	logger         *slog.Logger
	coreLogger     *slog.Logger
	Server         *atc_server.Server
	enabledModules map[string]bool
	otelShutdown   func(context.Context) error

	Forwarder  *forwarder.Forwarder
	Redirector *redirector.Redirector

	isLeader         atomic.Bool
	forwarderLeader  atomic.Bool
	redirectorLeader atomic.Bool
}

func New(cfg Config) (*Atc, error) {
	logger := initLogger(cfg.Server.LogFormat, cfg.Server.LogLevel)

	otelShutdown, err := telemetry.Init(context.Background(), "atc")
	if err != nil {
		logger.Warn("Failed to initialize OpenTelemetry SDK", slog.Any("error", err))
	}

	atc := &Atc{
		Cfg:            cfg,
		logger:         logger,
		coreLogger:     logger.With(slog.String("module", "core")),
		enabledModules: resolveModules(cfg.Target),
		otelShutdown:   otelShutdown,
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
	for _, module := range t.Cfg.Target {
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

	var shutdownRequested atomic.Bool

	if t.Server != nil {
		t.Server.Mux.HandleFunc("/health", t.healthHandler(&shutdownRequested))
		t.Server.Mux.Handle("GET /services", t.authMiddleware(http.HandlerFunc(t.servicesHandler)))
	}

	if t.Server != nil {
		g.Go(func() error {
			return t.Server.Run(ctx)
		})
	}

	if t.Cfg.HA.Enabled {
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
		shutdownRequested.Store(true)
	}()

	err := g.Wait()
	if err != nil {
		t.coreLogger.ErrorContext(ctx, "Application stopped with error", slog.Any("err", err))
	} else {
		t.coreLogger.InfoContext(ctx, "Application stopped gracefully")
	}

	return err
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

	lock, err := client.LockOpts(opts)
	if err != nil {
		return fmt.Errorf("failed to configure consul lock: %w", err)
	}

	t.coreLogger.InfoContext(ctx, "Starting leader election loop", slog.String("lock_key", lockKey), slog.String("module", moduleName))

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		t.coreLogger.InfoContext(ctx, "Attempting to acquire leadership lock...", slog.String("lock_key", lockKey))
		lostCh, err := lock.Lock(ctx.Done())
		if err != nil {
			t.coreLogger.ErrorContext(ctx, "Error acquiring leadership lock, retrying in 5s", slog.Any("error", err), slog.String("lock_key", lockKey))
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

		if runFunc != nil {
			leaderCtx, cancelLeader := context.WithCancel(ctx)

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := runFunc(leaderCtx); err != nil {
					t.coreLogger.ErrorContext(leaderCtx, "workload run failed", slog.Any("error", err), slog.String("lock_key", lockKey))
				}
			}()

			select {
			case <-ctx.Done():
				cancelLeader()
				_ = lock.Unlock()
				leaderFlag.Store(false)
				wg.Wait()
				return nil
			case <-lostCh:
				t.coreLogger.WarnContext(ctx, "Leadership lock lost. Stopping workload and reverting to standby...", slog.String("lock_key", lockKey))
				cancelLeader()
				leaderFlag.Store(false)
				wg.Wait()
			}
		} else {
			select {
			case <-ctx.Done():
				_ = lock.Unlock()
				leaderFlag.Store(false)
				return nil
			case <-lostCh:
				t.coreLogger.WarnContext(ctx, "Leadership lock lost. Reverting to standby...", slog.String("lock_key", lockKey))
				leaderFlag.Store(false)
			}
		}
	}
}

func (t *Atc) healthHandler(shutdownRequested *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if shutdownRequested.Load() {
			t.coreLogger.Debug("application is stopping")
			http.Error(w, "Application is stopping", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "OK")
	}
}

func (t *Atc) GetEnabledModulesTable() string {
	return RenderServicesTable(t.enabledModules)
}

func (t *Atc) GetAtcEnabledServices(ctx context.Context) ([]string, error) {
	client, err := t.getConsulClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to consul: %w", err)
	}

	services, _, err := client.Catalog().Services((&api.QueryOptions{}).WithContext(ctx))
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
	client, err := t.getConsulClient(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to connect to consul: %v", err), http.StatusInternalServerError)
		return
	}

	services, _, err := client.Catalog().Services((&api.QueryOptions{}).WithContext(r.Context()))
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch services: %v", err), http.StatusInternalServerError)
		return
	}

	// Fetch all service-resolver config entries to find deleted ones
	entries, _, err := client.ConfigEntries().List("service-resolver", (&api.QueryOptions{}).WithContext(r.Context()))
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list config entries: %v", err), http.StatusInternalServerError)
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
			if existing, exists := existingResolverEntries[svcName]; exists {
				entryMeta = existing.Meta
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
				Namespace:        t.Cfg.ConsulNamespace,
				Tags:             tags,
				ResolverType:     resType,
				Status:           "active",
				FailoverStrategy: failoverStrategy,
				RedirectStrategy: redirectStrategy,
				FailoverTargets:  failoverTargets,
				RedirectTarget:   redirectTarget,
				Meta:             entryMeta,
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
							ns = t.Cfg.ConsulNamespace
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

	entry, _, err := client.ConfigEntries().Get("service-resolver", name, nil)
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
		_, err = client.ConfigEntries().Delete("service-resolver", name, (&api.WriteOptions{}).WithContext(ctx))
		if err != nil {
			span.RecordError(err)
			return fmt.Errorf("failed to delete config entry: %w", err)
		}
	}

	return nil
}

func (t *Atc) ApplyFailoverOverride(ctx context.Context, service string, targetDc string, targetNs string, duration string) error {
	client, err := t.getConsulClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to consul: %w", err)
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
		_, _, err = client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
		if err != nil {
			return fmt.Errorf("failed to set failover override: %w", err)
		}
		t.coreLogger.InfoContext(ctx, "Applied manual failover override", slog.String("service", service), slog.String("target_dc", targetDc), slog.String("target_ns", targetNs))
	}

	return nil
}

func (t *Atc) TriggerManualRedirect(ctx context.Context, service string, redirectDc string, redirectNs string, duration string) error {
	client, err := t.getConsulClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to consul: %w", err)
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
		_, _, err = client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
		if err != nil {
			return fmt.Errorf("failed to set redirect override: %w", err)
		}
		t.coreLogger.InfoContext(ctx, "Applied manual redirect override", slog.String("service", service), slog.String("redirect_dc", redirectDc), slog.String("redirect_ns", redirectNs))
	}

	return nil
}

func (t *Atc) apiServicesDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	svcName := r.URL.Query().Get("name")
	if svcName == "" {
		http.Error(w, "Missing 'name' query parameter", http.StatusBadRequest)
		return
	}

	err := t.PurgeServiceResolver(r.Context(), svcName)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "404") {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if strings.Contains(err.Error(), "not created by ATC") {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
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

func (t *Atc) getConsulClient(ctx context.Context) (*api.Client, error) {
	t.cfgMu.RLock()
	addr := t.Cfg.ConsulAddr
	token := t.Cfg.ConsulToken
	dc := t.Cfg.ConsulDC
	ns := t.Cfg.ConsulNamespace
	t.cfgMu.RUnlock()

	if ctxToken, ok := ctx.Value(tokenContextKey).(string); ok && ctxToken != "" {
		isStaticKey := false
		t.cfgMu.RLock()
		for _, sk := range t.Cfg.Auth.StaticKeys {
			if sk == ctxToken {
				isStaticKey = true
				break
			}
		}
		t.cfgMu.RUnlock()
		if !isStaticKey {
			token = ctxToken
		}
	}

	return api.NewClient(consulCfg(addr, token, dc, ns))
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
	t.cfgMu.Lock()
	t.Cfg = cfg
	t.cfgMu.Unlock()

	ctx, span := coreTracer.Start(context.Background(), "ReloadConfig")
	defer span.End()
	span.AddEvent("config_reloaded", trace.WithAttributes(attribute.Bool("dry_run", cfg.DryRun)))

	t.coreLogger.InfoContext(ctx, "Configuration reloaded dynamically from file watcher", slog.Bool("dry_run", cfg.DryRun))

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
		t.Redirector.UpdateConfig(
			cfg.Strategies.Redirect,
			cfg.DampeningPeriod,
			cfg.MinDampeningPeriod,
			cfg.ConsulNamespace,
			cfg.WriteRateLimit,
			cfg.DryRun,
		)
	}
}

func (t *Atc) TriggerConfigReload() error {
	t.coreLogger.Info("Triggering manual configuration reload")
	if configFile := viper.GetString("config"); configFile != "" {
		if err := viper.ReadInConfig(); err != nil {
			return fmt.Errorf("failed to read config file %s: %w", configFile, err)
		}
	}

	var newCfg Config
	if err := viper.Unmarshal(&newCfg); err != nil {
		return fmt.Errorf("failed to unmarshal configuration: %w", err)
	}

	if newCfg.Server.HTTPListenPort == 0 {
		newCfg.Server.HTTPListenPort = viper.GetInt("port")
	}
	if newCfg.Server.MetricsListenPort == 0 {
		newCfg.Server.MetricsListenPort = viper.GetInt("metrics_port")
	}
	if len(newCfg.Target) == 0 {
		newCfg.Target = viper.GetStringSlice("target")
	}
	if newCfg.Server.LogLevel == "" {
		newCfg.Server.LogLevel = viper.GetString("log_level")
	}
	if newCfg.ConsulAddr == "" {
		newCfg.ConsulAddr = viper.GetString("consul_addr")
	}
	if newCfg.ConsulToken == "" {
		newCfg.ConsulToken = viper.GetString("consul_token")
	}
	if newCfg.ConsulDC == "" {
		newCfg.ConsulDC = viper.GetString("consul_dc")
	}
	if newCfg.ConsulNamespace == "" {
		newCfg.ConsulNamespace = viper.GetString("consul_namespace")
	}
	if newCfg.WriteRateLimit == "" {
		newCfg.WriteRateLimit = viper.GetString("write_rate_limit")
	}
	if !viper.IsSet("server.ui_enabled") {
		newCfg.Server.UiEnabled = viper.GetBool("ui_enabled")
	}
	if !viper.IsSet("server.mcp_enabled") {
		newCfg.Server.McpEnabled = viper.GetBool("mcp_enabled")
	}
	newCfg.Server.MetricsNamespace = "atc"

	t.ReloadConfig(newCfg)
	return nil
}

func (t *Atc) ListActiveOverrides(ctx context.Context) ([]map[string]any, error) {
	client, err := t.getConsulClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to consul: %w", err)
	}

	entries, _, err := client.ConfigEntries().List("service-resolver", (&api.QueryOptions{}).WithContext(ctx))
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
			if !t.IsLeader() {
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
						}
					}
					span.End()
				}
			}
		}
	}
}
