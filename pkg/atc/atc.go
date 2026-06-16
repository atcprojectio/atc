package atc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/attachmentgenie/atc/pkg/atc/forwarder"
	"github.com/attachmentgenie/atc/pkg/atc/redirector"
	atc_server "github.com/attachmentgenie/atc/pkg/atc/server"
	"github.com/attachmentgenie/atc/pkg/atc/telemetry"
	"github.com/hashicorp/consul/api"
)

type Config struct {
	Name        string            `yaml:"service"`
	Server      atc_server.Config `yaml:"server"`
	Target      []string          `yaml:"target"`
	ConsulAddr  string            `yaml:"consul_addr"`
	ConsulToken string            `yaml:"consul_token"`
	ConsulDC    string            `yaml:"consul_dc"`
}

type Atc struct {
	Cfg            Config
	logger         *slog.Logger
	coreLogger     *slog.Logger
	Server         *atc_server.Server
	enabledModules map[string]bool
	otelShutdown   func(context.Context) error

	Forwarder  *forwarder.Forwarder
	Redirector *redirector.Redirector
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

	return atc, nil
}

func (t *Atc) Run() error {
	for _, module := range t.Cfg.Target {
		if !slices.Contains(UserVisibleModules, module) {
			return fmt.Errorf("selected target (%s) is an internal module or invalid target, which is not allowed", module)
		}
	}

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, ctx := errgroup.WithContext(signalCtx)

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
		t.Server.Mux.HandleFunc("GET /services", t.servicesHandler)
	}

	if t.Server != nil {
		g.Go(func() error {
			return t.Server.Run(ctx)
		})
	}
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

	t.coreLogger.InfoContext(ctx, "Application started")

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
	client, err := api.NewClient(consulCfg(t.Cfg.ConsulAddr, t.Cfg.ConsulToken, t.Cfg.ConsulDC))
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
	client, err := api.NewClient(consulCfg(t.Cfg.ConsulAddr, t.Cfg.ConsulToken, t.Cfg.ConsulDC))
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

	type ServiceItem struct {
		Name         string   `json:"name"`
		Tags         []string `json:"tags"`
		ResolverType string   `json:"resolver_type"`
		Status       string   `json:"status"` // "active" or "deleted"
	}

	resultMap := make(map[string]ServiceItem)

	// 1. Process active catalog services
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			resType := "none"
			if existing, exists := existingResolverEntries[svcName]; exists {
				if len(existing.Failover) > 0 {
					resType = "failover"
				} else if existing.Redirect != nil && existing.Redirect.Service != "" {
					resType = "redirect"
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
				Name:         svcName,
				Tags:         tags,
				ResolverType: resType,
				Status:       "active",
			}
		}
	}

	// 2. Process config entries to find deleted services that are being redirected
	for _, entry := range entries {
		if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
			if res.Meta != nil && res.Meta["created-by"] == "atc" {
				if _, active := resultMap[res.Name]; !active {
					// Check if completely absent from catalog
					if _, exists := services[res.Name]; !exists {
						resType := "redirect"
						if len(res.Failover) > 0 {
							resType = "failover"
						}
						resultMap[res.Name] = ServiceItem{
							Name:         res.Name,
							Tags:         []string{"atc.enabled=true", "status:deleted"},
							ResolverType: resType,
							Status:       "deleted",
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
	client, err := api.NewClient(consulCfg(t.Cfg.ConsulAddr, t.Cfg.ConsulToken, t.Cfg.ConsulDC))
	if err != nil {
		return fmt.Errorf("failed to connect to consul: %w", err)
	}

	entry, _, err := client.ConfigEntries().Get("service-resolver", name, nil)
	if err != nil {
		return fmt.Errorf("failed to fetch config entry: %w", err)
	}

	resolver, ok := entry.(*api.ServiceResolverConfigEntry)
	if !ok || resolver.Meta == nil || resolver.Meta["created-by"] != "atc" {
		return fmt.Errorf("entry was not created by ATC")
	}

	_, err = client.ConfigEntries().Delete("service-resolver", name, (&api.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to delete config entry: %w", err)
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

	w.WriteHeader(http.StatusNoContent)
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
