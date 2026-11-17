package atc

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

func extractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		clientIP := strings.Split(xff, ",")[0]
		clientIP = strings.TrimSpace(clientIP)
		if clientIP != "" {
			return clientIP
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (t *Atc) LogAudit(ctx context.Context, action, service string, details map[string]any) {
	t.logAudit(ctx, nil, action, service, details)
}

func (t *Atc) logAudit(ctx context.Context, r *http.Request, action, service string, details map[string]any) {
	clientIP := extractClientIP(r)
	if clientIP == "" {
		if ctxIP, ok := ctx.Value(clientIPContextKey).(string); ok {
			clientIP = ctxIP
		}
	}

	token := "unknown"
	if ctxToken, ok := ctx.Value(tokenContextKey).(string); ok && ctxToken != "" {
		token = maskToken(ctxToken)
	}

	// Build slog fields
	args := []any{
		slog.String("module", "audit"),
		slog.Bool("audit", true),
		slog.String("action", action),
		slog.String("service", service),
		slog.String("actor", token),
		slog.String("client_ip", clientIP),
	}

	for k, v := range details {
		args = append(args, slog.Any(k, v))
	}

	t.logger.InfoContext(ctx, "Audit Log Event", args...)
}

func maskToken(token string) string {
	if len(token) <= 8 {
		return "***"
	}
	return token[:3] + "..." + token[len(token)-3:]
}
