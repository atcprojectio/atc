package server

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certReloader serves a certificate from disk and reloads it when either file changes, so
// rotated certificates (e.g. from cert-manager or Vault) are picked up without a restart.
type certReloader struct {
	certFile, keyFile string
	logger            *slog.Logger

	mu      sync.Mutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
}

func newCertReloader(certFile, keyFile string, logger *slog.Logger) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, logger: logger}
	if err := r.reloadIfChanged(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certReloader) reloadIfChanged() error {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return fmt.Errorf("tls certificate: %w", err)
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return fmt.Errorf("tls key: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cert != nil && certInfo.ModTime().Equal(r.certMod) && keyInfo.ModTime().Equal(r.keyMod) {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("loading tls certificate and key: %w", err)
	}
	if r.cert != nil {
		r.logger.Info("Reloaded TLS certificate", slog.String("cert_file", r.certFile))
	}
	r.cert, r.certMod, r.keyMod = &cert, certInfo.ModTime(), keyInfo.ModTime()
	return nil
}

// GetCertificate implements tls.Config.GetCertificate. If a reload fails (for example while
// a rotation is half-written), the previous certificate keeps being served.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if err := r.reloadIfChanged(); err != nil {
		r.logger.Warn("Keeping the current TLS certificate; reload failed", slog.Any("error", err))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cert, nil
}

func (r *certReloader) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.GetCertificate}
}
