package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	prom "github.com/prometheus/client_golang/prometheus"
	versioncol "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// GlobalPrometheusHandler is stored so that modules.go can retrieve and serve it.
var GlobalPrometheusHandler http.Handler

// Init initializes the OpenTelemetry SDK (traces, metrics, and logs).
// It returns a shutdown function that should be called when the application exits.
// OTLPEnabled reports whether OTLP export is configured for a signal ("TRACES", "METRICS" or
// "LOGS"): OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT must be set and
// OTEL_SDK_DISABLED must not be "true". Without an endpoint, the exporters used to default to
// localhost:4318 and log an error every export interval when no collector was running.
// Prometheus metrics on the metrics port are always available.
func OTLPEnabled(signal string) bool {
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") != ""
}

func Init(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	var shutdownFuncs []func(context.Context) error

	shutdown := func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		return err
	}

	// 1. Resource configuration
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			"",
			attribute.String("service.name", serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTel resource: %w", err)
	}

	// 2. Set up propagators (W3C Trace Context and Baggage)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// 3. Tracing setup (OTLP HTTP)
	if OTLPEnabled("TRACES") {
		traceExporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(traceExporter),
			sdktrace.WithResource(res),
		)
		otel.SetTracerProvider(tp)
		shutdownFuncs = append(shutdownFuncs, tp.Shutdown)
	}

	// 4. Metrics setup
	var metricReaders []sdkmetric.Reader
	if OTLPEnabled("METRICS") {
		metricExporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create OTLP metric exporter: %w", err)
		}
		periodicReader := sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(15*time.Second))
		metricReaders = append(metricReaders, periodicReader)
	}

	registry := prom.NewRegistry()
	registry.MustRegister(versioncol.NewCollector("atc"))
	promExporter, err := prometheus.New(
		prometheus.WithRegisterer(registry),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Prometheus exporter: %w", err)
	}
	GlobalPrometheusHandler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	metricReaders = append(metricReaders, promExporter)

	var metricOpts []sdkmetric.Option
	metricOpts = append(metricOpts, sdkmetric.WithResource(res))
	for _, reader := range metricReaders {
		metricOpts = append(metricOpts, sdkmetric.WithReader(reader))
	}
	mp := sdkmetric.NewMeterProvider(metricOpts...)
	otel.SetMeterProvider(mp)
	shutdownFuncs = append(shutdownFuncs, mp.Shutdown)

	// 5. Logging setup (OTLP HTTP)
	if OTLPEnabled("LOGS") {
		logExporter, err := otlploghttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create OTLP log exporter: %w", err)
		}
		lp := sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
			sdklog.WithResource(res),
		)
		otel.SetLoggerProvider(lp)
		shutdownFuncs = append(shutdownFuncs, lp.Shutdown)
	}

	return shutdown, nil
}
