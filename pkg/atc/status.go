package atc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

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

func (t *Atc) apiLeaderHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	leader := t.IsLeader()
	_, _ = fmt.Fprintf(w, `{"leader":%t}`, leader)
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
	Service  string `json:"service"`
	Type     string `json:"type"`
	TargetDc string `json:"target_dc"`
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
		err = t.ApplyFailoverOverride(r.Context(), req.Service, req.TargetDc)
	case "redirect":
		err = t.TriggerManualRedirect(r.Context(), req.Service, req.TargetDc)
	default:
		http.Error(w, fmt.Sprintf("invalid override type %q, must be 'failover' or 'redirect'", req.Type), http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, fmt.Sprintf("failed to apply override: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, `{"status":"applied"}`)
}
