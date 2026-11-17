package forwarder

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"

	"github.com/hashicorp/consul/api"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// AdoptTag lets ATC take over a service-resolver it did not create.
const AdoptTag = "atc.adopt=true"

// Values of the "created-by" metadata key on resolvers written by ATC.
const (
	CreatedByATC      = "atc"
	CreatedByOverride = "atc-override"
)

// IsATCOwned reports whether a resolver was written by ATC, automatically or as an override.
func IsATCOwned(e *api.ServiceResolverConfigEntry) bool {
	if e == nil || e.Meta == nil {
		return false
	}
	return e.Meta["created-by"] == CreatedByATC || e.Meta["created-by"] == CreatedByOverride
}

// MayManage reports whether ATC may write the resolver for a service: when none exists, when
// ATC created it, or when the service is tagged atc.adopt=true. A resolver written by someone
// else is left alone, because ATC replaces whole entries and would drop fields it does not
// manage (subsets, load balancing, timeouts).
func MayManage(existing *api.ServiceResolverConfigEntry, exists bool, tags []string) bool {
	return !exists || IsATCOwned(existing) || slices.Contains(tags, AdoptTag)
}

// ForeignTracker remembers which services have a resolver ATC leaves alone, so each one is
// logged once rather than on every reconcile, and exposes the count as a gauge.
type ForeignTracker struct {
	mu       sync.Mutex
	module   string
	logger   *slog.Logger
	services map[string]struct{}
}

// NewForeignTracker creates a tracker and registers the atc_foreign_resolvers gauge.
func NewForeignTracker(meter metric.Meter, module string, logger *slog.Logger) *ForeignTracker {
	ft := &ForeignTracker{module: module, logger: logger, services: map[string]struct{}{}}
	_, _ = meter.Int64ObservableGauge(
		"atc_foreign_resolvers",
		metric.WithDescription("atc.enabled services whose existing service-resolver was not created by ATC and is left untouched"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			ft.mu.Lock()
			n := len(ft.services)
			ft.mu.Unlock()
			o.Observe(int64(n), metric.WithAttributes(attribute.String("module", ft.module)))
			return nil
		}),
	)
	return ft
}

// Update replaces the tracked set with the services found in the latest reconcile, logging
// services that became foreign since the previous one.
func (ft *ForeignTracker) Update(ctx context.Context, current map[string]struct{}) {
	if ft == nil {
		return
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	for svc := range current {
		if _, seen := ft.services[svc]; !seen {
			ft.logger.WarnContext(ctx, "Leaving existing service-resolver untouched: it was not created by ATC. Tag the service "+AdoptTag+" to let ATC manage it.",
				slog.String("service", svc))
		}
	}
	ft.services = current
}

// OwnerState summarises who controls a service-resolver: "" when it does not exist, otherwise
// "created-by=<value>" (with an empty value for resolvers written without ATC metadata).
func OwnerState(e *api.ServiceResolverConfigEntry, exists bool) string {
	if !exists || e == nil {
		return ""
	}
	createdBy := ""
	if e.Meta != nil {
		createdBy = e.Meta["created-by"]
	}
	return "created-by=" + createdBy
}

// CurrentOwnerState reads the resolver from Consul and returns its OwnerState.
func CurrentOwnerState(ctx context.Context, client *api.Client, service string) (string, error) {
	entry, _, err := client.ConfigEntries().Get(api.ServiceResolver, service, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		var se api.StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return "", nil
		}
		return "", err
	}
	res, _ := entry.(*api.ServiceResolverConfigEntry)
	return OwnerState(res, res != nil), nil
}
