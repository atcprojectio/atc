package forwarder

import (
	"io"
	"log/slog"
	"testing"
)

func BenchmarkCachedStrategiesLookup(b *testing.B) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	f, err := New(
		logger,
		"127.0.0.1:8500",
		"",
		"dc1",
		"",
		"1s",
		map[string]FailoverStrategy{
			"default": {
				ConnectTimeout: "5s",
				Targets: []FailoverTarget{
					{Service: "app", Datacenter: "dc2"},
				},
			},
		},
		"10s",
		"1s",
		true,
	)
	if err != nil {
		b.Fatalf("failed to create forwarder: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = f.GetCachedStrategies("test-service")
	}
}

func BenchmarkUpdateConfig(b *testing.B) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	f, err := New(
		logger,
		"127.0.0.1:8500",
		"",
		"dc1",
		"",
		"1s",
		map[string]FailoverStrategy{},
		"10s",
		"1s",
		true,
	)
	if err != nil {
		b.Fatalf("failed to create forwarder: %v", err)
	}

	newStrategies := map[string]FailoverStrategy{
		"strategy-1": {
			ConnectTimeout: "5s",
			Targets: []FailoverTarget{
				{Service: "svc-1", Datacenter: "dc2"},
			},
		},
		"strategy-2": {
			ConnectTimeout: "3s",
			Targets: []FailoverTarget{
				{Service: "svc-2", Datacenter: "dc3"},
			},
		},
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f.UpdateConfig(newStrategies, "10s", "1s", "", "1s", true)
	}
}
