package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/api/watch"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

type activeWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

type ConsulWatcher struct {
	mu              sync.RWMutex
	logger          *slog.Logger
	consulAddr      string
	consulToken     string
	consulDC        string
	consulNamespace string
	Events          *Broadcaster

	runMu         sync.Mutex
	activeRunners int
	currentWatch  *activeWatch

	eventsCounter metric.Int64Counter
	errorsCounter metric.Int64Counter
}

func New(logger *slog.Logger, consulAddr, consulToken, consulDC, consulNamespace string) *ConsulWatcher {
	meter := otel.Meter("atc/watcher")
	eventsCounter, _ := meter.Int64Counter(
		"atc_watcher_events_total",
		metric.WithDescription("Total number of watcher events received from Consul"),
	)
	errorsCounter, _ := meter.Int64Counter(
		"atc_watcher_errors_total",
		metric.WithDescription("Total number of watcher errors or terminations"),
	)

	return &ConsulWatcher{
		logger:          logger,
		consulAddr:      consulAddr,
		consulToken:     consulToken,
		consulDC:        consulDC,
		consulNamespace: consulNamespace,
		Events:          NewBroadcaster(),
		eventsCounter:   eventsCounter,
		errorsCounter:   errorsCounter,
	}
}

func (w *ConsulWatcher) SetNamespace(ns string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.consulNamespace = ns
}

var tracer = otel.Tracer("atc/watcher")

func (w *ConsulWatcher) Run(ctx context.Context) error {
	w.runMu.Lock()
	aw := w.currentWatch
	if aw != nil {
		select {
		case <-aw.done:
			aw = nil
		default:
		}
	}
	if aw == nil {
		watchCtx, watchCancel := context.WithCancel(context.Background())
		aw = &activeWatch{
			cancel: watchCancel,
			done:   make(chan struct{}),
		}
		w.currentWatch = aw
		w.activeRunners = 1
		go func() {
			aw.err = w.runWatchPlans(watchCtx)
			close(aw.done)
		}()
	} else {
		w.activeRunners++
	}
	w.runMu.Unlock()

	select {
	case <-ctx.Done():
		w.runMu.Lock()
		w.activeRunners--
		shouldStop := w.activeRunners <= 0 && w.currentWatch == aw
		if shouldStop {
			w.currentWatch = nil
			w.activeRunners = 0
		}
		w.runMu.Unlock()
		if shouldStop {
			aw.cancel()
			<-aw.done
		}
		return nil
	case <-aw.done:
		w.runMu.Lock()
		if w.currentWatch == aw {
			w.currentWatch = nil
			w.activeRunners = 0
		}
		w.runMu.Unlock()
		return aw.err
	}
}

func (w *ConsulWatcher) runWatchPlans(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "ConsulWatcher.Run")
	defer span.End()

	w.mu.RLock()
	addr := w.consulAddr
	token := w.consulToken
	dc := w.consulDC
	ns := w.consulNamespace
	w.mu.RUnlock()

	client, err := api.NewClient(consulCfg(addr, token, dc, ns))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to connect to consul: %w", err)
	}

	servicesParams := map[string]any{"type": "services"}
	if dc != "" {
		servicesParams["datacenter"] = dc
	}
	if token != "" {
		servicesParams["token"] = token
	}
	servicesWatcher, err := watch.Parse(servicesParams)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to create services watcher plan: %w", err)
	}
	servicesWatcher.HybridHandler = func(_ watch.BlockingParamVal, _ any) {
		w.logger.DebugContext(ctx, "Watcher received services update from Consul")
		span.AddEvent("services_update_received")
		if w.eventsCounter != nil {
			w.eventsCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("event_type", "services_update"),
			))
		}
		w.Events.Broadcast("services_update")
	}

	checksParams := map[string]any{"type": "checks"}
	if dc != "" {
		checksParams["datacenter"] = dc
	}
	if token != "" {
		checksParams["token"] = token
	}
	checksWatcher, err := watch.Parse(checksParams)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to create checks watcher plan: %w", err)
	}
	checksWatcher.HybridHandler = func(_ watch.BlockingParamVal, _ any) {
		w.logger.DebugContext(ctx, "Watcher received checks update from Consul")
		span.AddEvent("checks_update_received")
		if w.eventsCounter != nil {
			w.eventsCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("event_type", "checks_update"),
			))
		}
		w.Events.Broadcast("checks_update")
	}

	errChan := make(chan error, 2)

	defer func() {
		servicesWatcher.Stop()
		checksWatcher.Stop()
	}()

	w.logger.InfoContext(ctx, "Starting services and checks watcher routines")
	span.AddEvent("watcher_routines_started")

	go func() {
		errChan <- servicesWatcher.RunWithClientAndHclog(client, servicesWatcher.Logger)
	}()

	go func() {
		errChan <- checksWatcher.RunWithClientAndHclog(client, checksWatcher.Logger)
	}()

	select {
	case <-ctx.Done():
		w.logger.InfoContext(ctx, "Watcher loop shutting down gracefully")
		span.AddEvent("watcher_shutdown_graceful")
		return nil
	case err = <-errChan:
		if w.errorsCounter != nil {
			w.errorsCounter.Add(ctx, 1)
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		w.logger.ErrorContext(ctx, "Consul watcher routine terminated", slog.Any("error", err))
		return fmt.Errorf("services or checks watcher terminated: %w", err)
	}
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
