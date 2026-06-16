package atc

import (
	"fmt"
	"net/http"
	"slices"

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
