package atc

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

func (t *Atc) logAudit(ctx context.Context, r *http.Request, action, service string, details map[string]any) {
	clientIP := ""
	if r != nil {
		xff := r.Header.Get("X-Forwarded-For")
		if xff != "" {
			clientIP = strings.Split(xff, ",")[0]
			clientIP = strings.TrimSpace(clientIP)
		}
		if clientIP == "" {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err == nil {
				clientIP = host
			} else {
				clientIP = r.RemoteAddr
			}
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
