package mcp

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NOTE: For every HTTP API endpoint exposed by the ATC server, a corresponding MCP tool MUST be registered here.

type Service interface {
	GetEnabledModulesTable() string
	GetAtcEnabledServices(ctx context.Context) ([]string, error)
	PurgeServiceResolver(ctx context.Context, name string) error
	IsLeader() bool
	GetFederationStatus(ctx context.Context) (map[string]string, error)
	ApplyFailoverOverride(ctx context.Context, service string, targetDc string) error
	TriggerManualRedirect(ctx context.Context, service string, redirectDc string) error
}

func NewHandler(svc Service) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "atc",
		Version: "v0.0.4",
	}, nil)

	// Tool mapping for /ready API endpoint
	mcp.AddTool(server, &mcp.Tool{
		Name:        "check_readiness",
		Description: "Checks the readiness state of the ATC service (corresponds to /ready endpoint)",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: "OK",
				},
			},
		}, nil, nil
	})

	// Tool mapping for /api/leader API endpoint
	mcp.AddTool(server, &mcp.Tool{
		Name:        "check_leadership",
		Description: "Checks if this ATC instance is the active leader coordinating Consul reconciliation (corresponds to /api/leader endpoint)",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, any, error) {
		leader := svc.IsLeader()
		var output string
		if leader {
			output = "This instance is the ACTIVE LEADER."
		} else {
			output = "This instance is in STANDBY mode."
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: output,
				},
			},
		}, nil, nil
	})

	// Tool mapping for /api/services API endpoint
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_atc_enabled_services",
		Description: "Lists all Consul services configured with the 'atc.enabled=true' tag (corresponds to /api/services endpoint)",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, any, error) {
		res, err := svc.GetAtcEnabledServices(ctx)
		if err != nil {
			return nil, nil, err
		}
		var output string
		if len(res) == 0 {
			output = "No services configured with 'atc.enabled=true' tag."
		} else {
			output = "Services configured with 'atc.enabled=true':\n"
			for _, s := range res {
				output += "- " + s + "\n"
			}
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: output,
				},
			},
		}, nil, nil
	})

	// Tool mapping for /api/federation API endpoint
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_wan_federation_status",
		Description: "Lists all WAN-federated datacenters and their connection status (corresponds to /api/federation endpoint)",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, any, error) {
		dcMap, err := svc.GetFederationStatus(ctx)
		if err != nil {
			return nil, nil, err
		}
		var output string
		if len(dcMap) == 0 {
			output = "No WAN-federated datacenters detected."
		} else {
			output = "WAN Federation Status:\n"
			for dc, status := range dcMap {
				output += fmt.Sprintf("- %s: %s\n", dc, strings.ToUpper(status))
			}
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: output,
				},
			},
		}, nil, nil
	})

	// Tool mapping to purge a service resolver config entry
	mcp.AddTool(server, &mcp.Tool{
		Name:        "purge_redirect_config",
		Description: "Purges the redirect/failover service-resolver config entry for a given service in Consul if created by ATC",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input struct {
		Name string `json:"name"`
	}) (*mcp.CallToolResult, any, error) {
		err := svc.PurgeServiceResolver(ctx, input.Name)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Successfully purged service-resolver config entry for %s", input.Name),
				},
			},
		}, nil, nil
	})

	// Tool mapping for applying manual failover override
	mcp.AddTool(server, &mcp.Tool{
		Name:        "apply_failover_override",
		Description: "Applies a manual traffic failover override for a service to a target datacenter, bypassing automated reconciliation",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input struct {
		Service  string `json:"service"`
		TargetDc string `json:"target_dc"`
	}) (*mcp.CallToolResult, any, error) {
		err := svc.ApplyFailoverOverride(ctx, input.Service, input.TargetDc)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Successfully applied failover override for service %s to datacenter %s", input.Service, input.TargetDc),
				},
			},
		}, nil, nil
	})

	// Tool mapping for triggering manual redirect override
	mcp.AddTool(server, &mcp.Tool{
		Name:        "trigger_manual_redirect",
		Description: "Triggers a manual traffic redirect override for a service to a redirect datacenter, bypassing automated reconciliation",
	}, func(ctx context.Context, req *mcp.CallToolRequest, input struct {
		Service    string `json:"service"`
		RedirectDc string `json:"redirect_dc"`
	}) (*mcp.CallToolResult, any, error) {
		err := svc.TriggerManualRedirect(ctx, input.Service, input.RedirectDc)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Successfully triggered manual redirect for service %s to datacenter %s", input.Service, input.RedirectDc),
				},
			},
		}, nil, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)

	return corsMiddleware(handler)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Mcp-Session-Id")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
