package forwarder

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/attachmentgenie/atc/pkg/atc/watcher"
	"github.com/hashicorp/consul/api"
	"golang.org/x/sync/errgroup"
)

type Forwarder struct {
	logger      *slog.Logger
	consulAddr  string
	consulToken string
	consulDC    string
	watcher     *watcher.ConsulWatcher
}

func New(logger *slog.Logger, consulAddr, consulToken, consulDC string) (*Forwarder, error) {
	return &Forwarder{
		logger:      logger,
		consulAddr:  consulAddr,
		consulToken: consulToken,
		consulDC:    consulDC,
		watcher:     watcher.New(logger, consulAddr, consulToken, consulDC),
	}, nil
}

func (f *Forwarder) Run(ctx context.Context) error {
	client, err := api.NewClient(consulCfg(f.consulAddr, f.consulToken, f.consulDC))
	if err != nil {
		return fmt.Errorf("failed to create consul client: %w", err)
	}

	events := f.watcher.Events.Subscribe()
	defer f.watcher.Events.Unsubscribe(events)

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return f.watcher.Run(ctx)
	})

	g.Go(func() error {
		f.logger.Info("Starting initial forwarder catalog reconciliation")
		if err := f.reconcile(ctx, client); err != nil {
			f.logger.Error("Initial reconciliation failed", slog.Any("error", err))
		}

		for {
			select {
			case <-ctx.Done():
				return nil
			case msg, ok := <-events:
				if !ok {
					return nil
				}
				f.logger.Info("Received watcher event, running reconciliation", slog.String("event", msg))
				if err := f.reconcile(ctx, client); err != nil {
					f.logger.Error("Reconciliation failed", slog.Any("error", err))
				}
			}
		}
	})

	return g.Wait()
}

func (f *Forwarder) reconcile(ctx context.Context, client *api.Client) error {
	services, _, err := client.Catalog().Services((&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to fetch consul services: %w", err)
	}

	localDC := getLocalDatacenter(client, f.consulDC)
	targetDC, err := findTargetDatacenter(client, localDC)
	if err != nil {
		return err
	}

	// 1. Fetch all existing config entries of type service-resolver
	entries, _, err := client.ConfigEntries().List("service-resolver", (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to list service-resolver config entries: %w", err)
	}

	existingResolverEntries := make(map[string]*api.ServiceResolverConfigEntry)
	for _, entry := range entries {
		if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
			existingResolverEntries[res.Name] = res
		}
	}

	// 2. Reconcile active catalog services
	for svcName, tags := range services {
		if slices.Contains(tags, "atc.enabled=true") {
			// Service is active locally and enabled.
			// It must have a Failover resolver entry.
			existing, exists := existingResolverEntries[svcName]
			needsCreateOrUpdate := !exists ||
				existing.Meta == nil ||
				existing.Meta["created-by"] != "atc" ||
				existing.Failover == nil ||
				len(existing.Failover) == 0

			// Also check if target DC matches
			if !needsCreateOrUpdate && existing.Failover != nil {
				fo, ok := existing.Failover["*"]
				if !ok || len(fo.Targets) != 1 || fo.Targets[0].Datacenter != targetDC || fo.Targets[0].Service != svcName {
					needsCreateOrUpdate = true
				}
			}

			if needsCreateOrUpdate {
				f.logger.Info("Creating/Updating failover service-resolver for active service", slog.String("service", svcName), slog.String("datacenter", targetDC))
				entry := &api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: svcName,
					Meta: map[string]string{
						"created-by": "atc",
					},
					ConnectTimeout: 15 * time.Second,
					Failover: map[string]api.ServiceResolverFailover{
						"*": {
							Targets: []api.ServiceResolverFailoverTarget{
								{
									Service:    svcName,
									Datacenter: targetDC,
								},
							},
						},
					},
				}
				_, _, err := client.ConfigEntries().Set(entry, (&api.WriteOptions{}).WithContext(ctx))
				if err != nil {
					f.logger.Error("Failed to set failover service-resolver", slog.String("service", svcName), slog.Any("error", err))
				}
			}
		} else {
			// Service exists locally but does not have the tag.
			// If we created a config entry, we must delete it.
			if existing, exists := existingResolverEntries[svcName]; exists {
				if existing.Meta != nil && existing.Meta["created-by"] == "atc" {
					f.logger.Info("Deleting service-resolver because atc.enabled tag was removed", slog.String("service", svcName))
					_, err := client.ConfigEntries().Delete("service-resolver", svcName, (&api.WriteOptions{}).WithContext(ctx))
					if err != nil {
						f.logger.Error("Failed to delete service-resolver", slog.String("service", svcName), slog.Any("error", err))
					}
				}
			}
		}
	}

	return nil
}

func getLocalDatacenter(client *api.Client, configDC string) string {
	if configDC != "" {
		return configDC
	}
	self, err := client.Agent().Self()
	if err == nil {
		if config, ok := self["Config"]; ok {
			if dc, ok := config["Datacenter"].(string); ok {
				return dc
			}
		}
	}
	return "dc1"
}

func findTargetDatacenter(client *api.Client, localDC string) (string, error) {
	dcs, err := client.Catalog().Datacenters()
	if err != nil {
		return "", fmt.Errorf("failed to fetch datacenters: %w", err)
	}
	for _, dc := range dcs {
		if dc != localDC {
			return dc, nil
		}
	}
	return "dc2", nil
}

func consulCfg(addr, token, dc string) *api.Config {
	cfg := api.DefaultConfig()
	if addr != "" {
		cfg.Address = addr
	}
	if token != "" {
		cfg.Token = token
	}
	if dc != "" {
		cfg.Datacenter = dc
	}
	return cfg
}
