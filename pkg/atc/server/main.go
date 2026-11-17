package server

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

//go:embed dist/*
var frontendFS embed.FS

type Config struct {
	HTTPListenPort    int    `yaml:"http_listen_port" mapstructure:"http_listen_port"`
	MetricsListenPort int    `yaml:"metrics_listen_port" mapstructure:"metrics_listen_port"`
	McpListenPort     int    `yaml:"mcp_port" mapstructure:"mcp_port"`
	LogFormat         string `yaml:"log_format" mapstructure:"log_format"`
	LogLevel          string `yaml:"log_level" mapstructure:"log_level"`
	UiEnabled         bool   `yaml:"ui_enabled" mapstructure:"ui_enabled"`
	McpEnabled        bool   `yaml:"mcp_enabled" mapstructure:"mcp_enabled"`
	// ListenAddress is the interface the HTTP API/UI and MCP listeners bind to.
	// Empty means all interfaces. The metrics listener always binds to all interfaces.
	ListenAddress string `yaml:"listen_address" mapstructure:"listen_address"`
	// AllowedOrigins lists browser origins (e.g. "https://ops.example.com") allowed to call
	// the API and MCP endpoints cross-origin. Same-origin requests are always allowed.
	AllowedOrigins []string `yaml:"allowed_origins" mapstructure:"allowed_origins"`
	// TLSCertFile and TLSKeyFile enable HTTPS on the API/UI and MCP listeners. Both or neither
	// must be set. The files are re-read when they change. The metrics listener stays HTTP.
	TLSCertFile string `yaml:"tls_cert_file" mapstructure:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file" mapstructure:"tls_key_file"`
}

// TLSEnabled reports whether the API/UI and MCP listeners serve HTTPS.
func (c Config) TLSEnabled() bool { return c.TLSCertFile != "" && c.TLSKeyFile != "" }

type Server struct {
	cfg        Config
	logger     *slog.Logger
	Mux        *http.ServeMux
	MetricsMux *http.ServeMux
	McpMux     *http.ServeMux

	// MainMiddleware and McpMiddleware, when set, wrap the main and MCP muxes respectively.
	MainMiddleware func(http.Handler) http.Handler
	McpMiddleware  func(http.Handler) http.Handler
}

func wrap(h http.Handler, mw func(http.Handler) http.Handler) http.Handler {
	if mw == nil {
		return h
	}
	return mw(h)
}

func New(cfg Config, logger *slog.Logger) (*Server, error) {
	mux := http.NewServeMux()
	metricsMux := http.NewServeMux()
	mcpMux := http.NewServeMux()

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
		McpMux:     mcpMux,
	}, nil
}

// withHSTS tells browsers to keep using HTTPS for this host.
func withHSTS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Run(ctx context.Context) error {
	var tlsConfig *tls.Config
	if s.cfg.TLSEnabled() {
		reloader, err := newCertReloader(s.cfg.TLSCertFile, s.cfg.TLSKeyFile, s.logger)
		if err != nil {
			return err
		}
		tlsConfig = reloader.tlsConfig()
	}
	mainHandler := wrap(s.Mux, s.MainMiddleware)
	mcpHandler := wrap(s.McpMux, s.McpMiddleware)
	if tlsConfig != nil {
		mainHandler = withHSTS(mainHandler)
		mcpHandler = withHSTS(mcpHandler)
	}
	serve := func(srv *http.Server) error {
		if tlsConfig != nil {
			srv.TLSConfig = tlsConfig
			return srv.ListenAndServeTLS("", "")
		}
		return srv.ListenAndServe()
	}

	mainAddr := net.JoinHostPort(s.cfg.ListenAddress, strconv.Itoa(s.cfg.HTTPListenPort))
	mainServer := &http.Server{
		Addr:              mainAddr,
		Handler:           otelhttp.NewHandler(mainHandler, "atc-server"),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	metricsAddr := fmt.Sprintf(":%d", s.cfg.MetricsListenPort)
	metricsServer := &http.Server{
		Addr:              metricsAddr,
		Handler:           s.MetricsMux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	var mcpServer *http.Server
	mcpEnabled := s.cfg.McpEnabled && s.cfg.McpListenPort > 0
	if mcpEnabled {
		mcpAddr := net.JoinHostPort(s.cfg.ListenAddress, strconv.Itoa(s.cfg.McpListenPort))
		mcpServer = &http.Server{
			Addr:              mcpAddr,
			Handler:           otelhttp.NewHandler(mcpHandler, "atc-mcp-server"),
			ReadHeaderTimeout: 3 * time.Second,
			ReadTimeout:       10 * time.Second,
			// WriteTimeout is intentionally disabled (0) because MCP SSE and streamable HTTP connections are long-lived.
			WriteTimeout: 0,
			IdleTimeout:  60 * time.Second,
		}
	}

	errChan := make(chan error, 3)

	go func() {
		s.logger.InfoContext(ctx, "HTTP server listening", slog.String("address", mainAddr), slog.Bool("tls", tlsConfig != nil))
		if err := serve(mainServer); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("main server error: %w", err)
		}
	}()

	go func() {
		s.logger.InfoContext(ctx, "Metrics server listening", slog.String("address", metricsAddr))
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("metrics server error: %w", err)
		}
	}()

	if mcpEnabled {
		go func() {
			mcpAddr := mcpServer.Addr
			s.logger.InfoContext(ctx, "MCP server listening", slog.String("address", mcpAddr), slog.Bool("tls", tlsConfig != nil))
			if err := serve(mcpServer); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errChan <- fmt.Errorf("mcp server error: %w", err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		s.logger.InfoContext(ctx, "shutting down HTTP, Metrics, and MCP servers")
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
		if mcpEnabled {
			if err := mcpServer.Shutdown(shutdownCtx); err != nil {
				if shutdownErr != nil {
					shutdownErr = fmt.Errorf("%v; mcp server shutdown error: %w", shutdownErr, err)
				} else {
					shutdownErr = fmt.Errorf("mcp server shutdown error: %w", err)
				}
			}
		}
		return shutdownErr
	case err := <-errChan:
		// Attempt to shut down the other running servers
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = mainServer.Shutdown(shutdownCtx)
		_ = metricsServer.Shutdown(shutdownCtx)
		if mcpEnabled {
			_ = mcpServer.Shutdown(shutdownCtx)
		}
		return err
	}
}
