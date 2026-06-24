package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

//go:embed dist/*
var frontendFS embed.FS

type Config struct {
	HTTPListenPort    int    `yaml:"http_listen_port" mapstructure:"http_listen_port"`
	MetricsListenPort int    `yaml:"metrics_listen_port" mapstructure:"metrics_listen_port"`
	MetricsNamespace  string `yaml:"metrics_namespace" mapstructure:"metrics_namespace"`
	LogFormat         string `yaml:"log_format" mapstructure:"log_format"`
	LogLevel          string `yaml:"log_level" mapstructure:"log_level"`
	UiEnabled         bool   `yaml:"ui_enabled" mapstructure:"ui_enabled"`
	McpEnabled        bool   `yaml:"mcp_enabled" mapstructure:"mcp_enabled"`
}

type Server struct {
	cfg        Config
	logger     *slog.Logger
	Mux        *http.ServeMux
	MetricsMux *http.ServeMux
}

func New(cfg Config, logger *slog.Logger) (*Server, error) {
	mux := http.NewServeMux()
	metricsMux := http.NewServeMux()

	if cfg.UiEnabled {
		sub, err := fs.Sub(frontendFS, "dist")
		if err != nil {
			return nil, fmt.Errorf("failed to create frontend sub FS: %w", err)
		}

		fileServer := http.FileServer(http.FS(sub))

		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			if path != "/" {
				f, err := sub.Open(strings.TrimPrefix(path, "/"))
				if err != nil {
					r.URL.Path = "/"
				} else {
					_ = f.Close()
				}
			}
			fileServer.ServeHTTP(w, r)
		})
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Web UI is disabled", http.StatusNotFound)
		})
	}

	return &Server{
		cfg:        cfg,
		logger:     logger,
		Mux:        mux,
		MetricsMux: metricsMux,
	}, nil
}

func (s *Server) Run(ctx context.Context) error {
	mainAddr := fmt.Sprintf(":%d", s.cfg.HTTPListenPort)
	mainServer := &http.Server{
		Addr:              mainAddr,
		Handler:           otelhttp.NewHandler(s.Mux, "atc-server"),
		ReadHeaderTimeout: 3 * time.Second,
	}

	metricsAddr := fmt.Sprintf(":%d", s.cfg.MetricsListenPort)
	metricsServer := &http.Server{
		Addr:              metricsAddr,
		Handler:           s.MetricsMux,
		ReadHeaderTimeout: 3 * time.Second,
	}

	errChan := make(chan error, 2)

	go func() {
		s.logger.InfoContext(ctx, "HTTP server listening", slog.String("address", mainAddr))
		if err := mainServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("main server error: %w", err)
		}
	}()

	go func() {
		s.logger.InfoContext(ctx, "Metrics server listening", slog.String("address", metricsAddr))
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("metrics server error: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		s.logger.InfoContext(ctx, "shutting down HTTP and Metrics servers")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var shutdownErr error
		if err := mainServer.Shutdown(shutdownCtx); err != nil {
			shutdownErr = fmt.Errorf("main server shutdown error: %w", err)
		}
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			if shutdownErr != nil {
				shutdownErr = fmt.Errorf("%v; metrics server shutdown error: %w", shutdownErr, err)
			} else {
				shutdownErr = fmt.Errorf("metrics server shutdown error: %w", err)
			}
		}
		return shutdownErr
	case err := <-errChan:
		// Attempt to shut down the other running server
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = mainServer.Shutdown(shutdownCtx)
		_ = metricsServer.Shutdown(shutdownCtx)
		return err
	}
}
