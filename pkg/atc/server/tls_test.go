package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WriteTestCert writes a self-signed certificate and key for cn and returns their paths.
func writeTestCert(t *testing.T, dir, cn string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certFile, keyFile
}

func servedCN(t *testing.T, r *certReloader) string {
	t.Helper()
	c, err := r.GetCertificate(&tls.ClientHelloInfo{})
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	require.NoError(t, err)
	return leaf.Subject.CommonName
}

func TestCertReloaderPicksUpRotatedCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeTestCert(t, dir, "first.example")
	r, err := newCertReloader(certFile, keyFile, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	assert.Equal(t, "first.example", servedCN(t, r))

	// Rotate (as cert-manager would) with a later modification time.
	writeTestCert(t, dir, "second.example")
	later := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(certFile, later, later))
	require.NoError(t, os.Chtimes(keyFile, later, later))
	assert.Equal(t, "second.example", servedCN(t, r), "rotated certificate must be served without a restart")

	// A broken rotation keeps the last good certificate.
	require.NoError(t, os.WriteFile(certFile, []byte("half-written"), 0o600))
	broken := later.Add(2 * time.Second)
	require.NoError(t, os.Chtimes(certFile, broken, broken))
	assert.Equal(t, "second.example", servedCN(t, r))
}

func TestNewCertReloaderRejectsMissingFiles(t *testing.T) {
	_, err := newCertReloader("/nonexistent/tls.crt", "/nonexistent/tls.key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	assert.Error(t, err)
}
