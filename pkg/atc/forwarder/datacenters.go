package forwarder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/hashicorp/consul/api"
)

// LocalDatacenter returns the configured datacenter, or asks the Consul agent. It returns an
// error rather than guessing: a wrong local DC could make ATC fail a service over to itself.
func LocalDatacenter(client *api.Client, configDC string) (string, error) {
	if configDC != "" {
		return configDC, nil
	}
	self, err := client.Agent().Self()
	if err != nil {
		return "", fmt.Errorf("failed to determine local datacenter: %w", err)
	}
	if cfg, ok := self["Config"]; ok {
		if dc, ok := cfg["Datacenter"].(string); ok && dc != "" {
			return dc, nil
		}
	}
	return "", errors.New("failed to determine local datacenter: agent did not report one")
}

// RemoteDatacenters lists the other known datacenters, nearest first (Consul orders them by
// estimated round-trip time).
func RemoteDatacenters(client *api.Client, localDC string) ([]string, error) {
	dcs, err := client.Catalog().Datacenters()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch datacenters: %w", err)
	}
	return slices.DeleteFunc(dcs, func(dc string) bool { return dc == localDC }), nil
}

// PickDatacenter chooses a target for a strategy that does not name one. It keeps the
// previously chosen DC while it is still available, so small changes in round-trip-time
// ordering do not flip resolvers between datacenters; otherwise it picks the nearest. It
// returns "" when there is no other datacenter.
func PickDatacenter(remote []string, preferred string) string {
	if preferred != "" && slices.Contains(remote, preferred) {
		return preferred
	}
	if len(remote) > 0 {
		return remote[0]
	}
	return ""
}

// WarnTracker logs a warning once per key while a condition persists, instead of on every
// reconcile. Update replaces the set of current keys after each reconcile.
type WarnTracker struct {
	mu     sync.Mutex
	logger *slog.Logger
	msg    string
	seen   map[string]struct{}
}

// NewWarnTracker creates a tracker that logs msg for each newly seen key.
func NewWarnTracker(logger *slog.Logger, msg string) *WarnTracker {
	return &WarnTracker{logger: logger, msg: msg, seen: map[string]struct{}{}}
}

// Update records the keys found in the latest reconcile; keys map a service to a detail.
func (w *WarnTracker) Update(ctx context.Context, current map[string]string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	next := make(map[string]struct{}, len(current))
	for svc, detail := range current {
		key := svc + "\x00" + detail
		next[key] = struct{}{}
		if _, ok := w.seen[key]; !ok {
			w.logger.WarnContext(ctx, w.msg, slog.String("service", svc), slog.String("detail", detail))
		}
	}
	w.seen = next
}
