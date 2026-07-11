package atc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/hashicorp/consul/api"
	"github.com/jedib0t/go-pretty/v6/table"
)

func OkHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "OK")
	}
}

func (t *Atc) servicesHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, RenderServicesTable(t.enabledModules))
}

func RenderServicesTable(enabledModules map[string]bool) string {
	svcNames := make([]string, 0, len(enabledModules))
	for name, enabled := range enabledModules {
		if enabled {
			svcNames = append(svcNames, name)
		}
	}
	slices.Sort(svcNames)

	x := table.NewWriter()
	x.AppendHeader(table.Row{"service name", "status"})

	for _, name := range svcNames {
		x.AppendRows([]table.Row{
			{name, "running"},
		})
	}

	x.AppendSeparator()
	return x.Render()
}

type LeaderDetails struct {
	IsLeader   bool   `json:"is_leader"`
	LeaderNode string `json:"leader_node,omitempty"`
	LockKey    string `json:"lock_key"`
	SessionID  string `json:"session_id,omitempty"`
}

func (t *Atc) GetLeaderDetails(ctx context.Context, moduleName string) (LeaderDetails, error) {
	t.cfgMu.RLock()
	lockKey := t.Cfg.HA.LockKey
	haEnabled := t.Cfg.HA.Enabled
	localNodeName := t.Cfg.Name
	t.cfgMu.RUnlock()

	if lockKey == "" {
		lockKey = "atc/leader/lock"
	}
	if moduleName != "" {
		lockKey = lockKey + "/" + moduleName
	}

	details := LeaderDetails{
		LockKey: lockKey,
	}

	if !haEnabled {
		isLocalActive := false
		switch moduleName {
		case "forwarder":
			isLocalActive = t.Forwarder != nil
		case "redirector":
			isLocalActive = t.Redirector != nil
		}
		details.IsLeader = isLocalActive
		if isLocalActive {
			details.LeaderNode = localNodeName
		}
		return details, nil
	}

	client, err := t.getConsulClient(ctx)
	if err != nil {
		return details, err
	}

	var pair *api.KVPair
	err = t.traceConsulCall(ctx, "get_leader_key", func() error {
		var err error
		pair, _, err = client.KV().Get(lockKey, nil)
		return err
	})
	if err != nil {
		return details, err
	}

	if pair != nil && pair.Session != "" {
		details.SessionID = pair.Session
		details.LeaderNode = string(pair.Value)
		details.IsLeader = (details.LeaderNode == localNodeName)
	}

	return details, nil
}

func (t *Atc) ForceUnlock(ctx context.Context, moduleName string) error {
	client, err := t.getConsulClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to consul: %w", err)
	}

	t.cfgMu.RLock()
	lockKey := t.Cfg.HA.LockKey
	t.cfgMu.RUnlock()

	if lockKey == "" {
		lockKey = "atc/leader/lock"
	}
	if moduleName != "" {
		lockKey = lockKey + "/" + moduleName
	}

	var pair *api.KVPair
	err = t.traceConsulCall(ctx, "get_leader_key", func() error {
		var err error
		pair, _, err = client.KV().Get(lockKey, nil)
		return err
	})
	if err != nil {
		return err
	}

	if pair == nil {
		return fmt.Errorf("no lock found for module %q", moduleName)
	}

	if pair.Session != "" {
		err = t.traceConsulCall(ctx, "destroy_session", func() error {
			_, err := client.Session().Destroy(pair.Session, nil)
			return err
		})
		if err != nil {
			return fmt.Errorf("failed to destroy consul session %q: %w", pair.Session, err)
		}
	} else {
		err = t.traceConsulCall(ctx, "delete_leader_key", func() error {
			_, err := client.KV().Delete(lockKey, nil)
			return err
		})
		if err != nil {
			return fmt.Errorf("failed to delete lock key %q: %w", lockKey, err)
		}
	}

	return nil
}

type LeaderStatusResponse struct {
	Leader      bool                     `json:"leader"`
	AuthEnabled bool                     `json:"auth_enabled"`
	Components  map[string]bool          `json:"components"`
	LocalNode   string                   `json:"local_node"`
	Modules     map[string]LeaderDetails `json:"modules"`
}

func (t *Atc) apiLeaderHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	
	leader := t.IsLeader()

	t.cfgMu.RLock()
	haEnabled := t.Cfg.HA.Enabled
	authEnabled := t.Cfg.Auth.Enabled
	localNode := t.Cfg.Name
	t.cfgMu.RUnlock()

	var forwarderLeader, redirectorLeader bool
	if !haEnabled {
		forwarderLeader = t.Forwarder != nil
		redirectorLeader = t.Redirector != nil
	} else {
		forwarderLeader = t.forwarderLeader.Load()
		redirectorLeader = t.redirectorLeader.Load()
	}

	ctx := r.Context()
	modules := make(map[string]LeaderDetails)
	for _, m := range []string{"forwarder", "redirector"} {
		details, err := t.GetLeaderDetails(ctx, m)
		if err == nil {
			modules[m] = details
		}
	}

	resp := LeaderStatusResponse{
		Leader:      leader,
		AuthEnabled: authEnabled,
		Components: map[string]bool{
			"forwarder":  forwarderLeader,
			"redirector": redirectorLeader,
		},
		LocalNode: localNode,
		Modules:   modules,
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (t *Atc) apiFederationHandler(w http.ResponseWriter, r *http.Request) {
	dcMap, err := t.GetFederationStatus(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to fetch federation status: %v", err), http.StatusInternalServerError)
		return
	}

	type DcStatus struct {
		Datacenter string `json:"datacenter"`
		Status     string `json:"status"`
	}

	result := make([]DcStatus, 0, len(dcMap))
	for dc, status := range dcMap {
		result = append(result, DcStatus{
			Datacenter: dc,
			Status:     status,
		})
	}

	slices.SortFunc(result, func(a, b DcStatus) int {
		return strings.Compare(a.Datacenter, b.Datacenter)
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}

type overrideRequest struct {
	Service   string `json:"service"`
	Type      string `json:"type"`
	TargetDc  string `json:"target_dc"`
	Namespace string `json:"namespace,omitempty"`
	Duration  string `json:"duration,omitempty"`
}

func (t *Atc) apiOverridesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req overrideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request payload: %v", err), http.StatusBadRequest)
		return
	}

	if req.Service == "" {
		http.Error(w, "missing 'service' field", http.StatusBadRequest)
		return
	}
	if req.TargetDc == "" {
		http.Error(w, "missing 'target_dc' field", http.StatusBadRequest)
		return
	}

	var err error
	switch req.Type {
	case "failover":
		err = t.ApplyFailoverOverride(r.Context(), req.Service, req.TargetDc, req.Namespace, req.Duration)
	case "redirect":
		err = t.TriggerManualRedirect(r.Context(), req.Service, req.TargetDc, req.Namespace, req.Duration)
	default:
		http.Error(w, fmt.Sprintf("invalid override type %q, must be 'failover' or 'redirect'", req.Type), http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, fmt.Sprintf("failed to apply override: %v", err), http.StatusInternalServerError)
		return
	}

	t.logAudit(r.Context(), r, "create_override", req.Service, map[string]any{"type": req.Type, "target_dc": req.TargetDc, "namespace": req.Namespace})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, `{"status":"applied"}`)
}

func (t *Atc) apiStrategiesHandler(w http.ResponseWriter, r *http.Request) {
	t.cfgMu.RLock()
	strategies := t.Cfg.Strategies
	t.cfgMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(strategies)
}

func (t *Atc) apiModulesHandler(w http.ResponseWriter, r *http.Request) {
	t.cfgMu.RLock()
	defer t.cfgMu.RUnlock()

	var active []string
	for _, name := range UserVisibleModules {
		if name == Consul || name == All {
			continue
		}
		if t.enabledModules[name] {
			active = append(active, name)
		}
	}
	slices.Sort(active)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(active)
}

func (t *Atc) apiReloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := t.TriggerConfigReload()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"status":"error","message":"failed to reload config: %v"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"success","message":"Configuration reloaded successfully"}`))
}


