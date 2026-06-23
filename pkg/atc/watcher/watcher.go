package watcher

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/api/watch"
)

type ConsulWatcher struct {
	logger          *slog.Logger
	consulAddr      string
	consulToken     string
	consulDC        string
	consulNamespace string
	Events          *Broadcaster
}

func New(logger *slog.Logger, consulAddr, consulToken, consulDC, consulNamespace string) *ConsulWatcher {
	return &ConsulWatcher{
		logger:          logger,
		consulAddr:      consulAddr,
		consulToken:     consulToken,
		consulDC:        consulDC,
		consulNamespace: consulNamespace,
		Events:          NewBroadcaster(),
	}
}

func (w *ConsulWatcher) Run(ctx context.Context) error {
	client, err := api.NewClient(consulCfg(w.consulAddr, w.consulToken, w.consulDC, w.consulNamespace))
	if err != nil {
		return fmt.Errorf("failed to connect to consul: %w", err)
	}

	servicesParams := map[string]any{"type": "services"}
	servicesWatcher, err := watch.Parse(servicesParams)
	if err != nil {
		return fmt.Errorf("failed to create services watcher plan: %w", err)
	}
	servicesWatcher.HybridHandler = func(_ watch.BlockingParamVal, _ any) {
		w.Events.Broadcast("services_update")
	}

	checksParams := map[string]any{"type": "checks"}
	checksWatcher, err := watch.Parse(checksParams)
	if err != nil {
		return fmt.Errorf("failed to create checks watcher plan: %w", err)
	}
	checksWatcher.HybridHandler = func(_ watch.BlockingParamVal, _ any) {
		w.Events.Broadcast("checks_update")
	}

	errChan := make(chan error, 2)

	defer func() {
		servicesWatcher.Stop()
		checksWatcher.Stop()
	}()

	go func() {
		errChan <- servicesWatcher.RunWithClientAndHclog(client, servicesWatcher.Logger)
	}()

	go func() {
		errChan <- checksWatcher.RunWithClientAndHclog(client, checksWatcher.Logger)
	}()

	select {
	case <-ctx.Done():
		return nil
	case err = <-errChan:
		return fmt.Errorf("services or checks watcher terminated: %w", err)
	}
}

func consulCfg(addr, token, dc, ns string) *api.Config {
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
	if ns != "" {
		cfg.Namespace = ns
	}
	return cfg
}
