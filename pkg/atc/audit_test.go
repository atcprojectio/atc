package atc

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

type captureHandler struct {
	records []slog.Record
}

func (h *captureHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return h
}

func TestLogAudit(t *testing.T) {
	handler := &captureHandler{}
	logger := slog.New(handler)

	atc := &Atc{
		logger: logger,
	}

	// 1. Test basic audit log with token in context and X-Forwarded-For header
	ctx := context.WithValue(context.Background(), tokenContextKey, "secret-token-value-12345")
	req := httptest.NewRequest("POST", "/api/overrides", nil)
	req.Header.Set("X-Forwarded-For", "192.168.1.50, 10.0.0.1")

	details := map[string]any{
		"target_dc": "dc2",
		"duration":  "5m",
	}

	atc.logAudit(ctx, req, "apply_override", "payment-service", details)

	assert.Len(t, handler.records, 1)
	record := handler.records[0]
	assert.Equal(t, "Audit Log Event", record.Message)

	attrs := make(map[string]any)
	record.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})

	assert.Equal(t, "audit", attrs["module"])
	assert.Equal(t, true, attrs["audit"])
	assert.Equal(t, "apply_override", attrs["action"])
	assert.Equal(t, "payment-service", attrs["service"])
	assert.Equal(t, "sec...345", attrs["actor"])
	assert.Equal(t, "192.168.1.50", attrs["client_ip"])
	assert.Equal(t, "dc2", attrs["target_dc"])
	assert.Equal(t, "5m", attrs["duration"])

	// 2. Test audit log with RemoteAddr fallback and no token
	handler.records = nil
	ctx2 := context.Background()
	req2 := httptest.NewRequest("POST", "/api/overrides", nil)
	req2.RemoteAddr = "10.10.10.10:12345"

	atc.logAudit(ctx2, req2, "purge_resolver", "billing-service", nil)

	assert.Len(t, handler.records, 1)
	record2 := handler.records[0]
	attrs2 := make(map[string]any)
	record2.Attrs(func(a slog.Attr) bool {
		attrs2[a.Key] = a.Value.Any()
		return true
	})

	assert.Equal(t, "unknown", attrs2["actor"])
	assert.Equal(t, "10.10.10.10", attrs2["client_ip"])
	assert.Equal(t, "purge_resolver", attrs2["action"])
	assert.Equal(t, "billing-service", attrs2["service"])
}

func TestMaskToken(t *testing.T) {
	assert.Equal(t, "***", maskToken("1234567"))
	assert.Equal(t, "***", maskToken("12345678"))
	assert.Equal(t, "123...789", maskToken("123456789"))
	assert.Equal(t, "abc...xyz", maskToken("abcdefghijklmnopqrstuvwxyz"))
}
