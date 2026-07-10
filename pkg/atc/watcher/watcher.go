package watcher

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/api/watch"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

type ConsulWatcher struct {
	logger          *slog.Logger
	consulAddr      string
	consulToken     string
	consulDC        string
	consulNamespace string
	Events          *Broadcaster

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

var tracer = otel.Tracer("atc/watcher")

func (w *ConsulWatcher) Run(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "ConsulWatcher.Run")
	defer span.End()

	client, err := api.NewClient(consulCfg(w.consulAddr, w.consulToken, w.consulDC, w.consulNamespace))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to connect to consul: %w", err)
	}

	servicesParams := map[string]any{"type": "services"}
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
