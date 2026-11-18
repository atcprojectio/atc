// Package telemetrytest holds test helpers for metrics exported by the telemetry package.
package telemetrytest

import (
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/atcprojectio/atc/pkg/atc/telemetry"
)

// AssertSecondBuckets scrapes the Prometheus handler and checks that histogram name exists under
// its exact name (no unit suffix appended) and that its single sample lies in (below, upper]:
// the bucket with le=below holds no sample and the one with le=upper holds it. "0" for below
// means no lower boundary to check.
func AssertSecondBuckets(t testing.TB, name, below, upper string) {
	t.Helper()
	rr := httptest.NewRecorder()
	telemetry.GlobalPrometheusHandler.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()

	if strings.Contains(body, name+"_seconds") {
		t.Errorf("%s was renamed with a duplicated unit suffix", name)
	}
	count := func(le string) string {
		re := regexp.MustCompile(fmt.Sprintf(`(?m)^%s_bucket\{[^}]*le="%s"[^}]*\} (\S+)$`, regexp.QuoteMeta(name), regexp.QuoteMeta(le)))
		m := re.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("no %s bucket with le=%q in scrape:\n%s", name, le, grep(body, name))
		}
		return m[1]
	}
	if below != "0" {
		if got := count(below); got != "0" {
			t.Errorf("%s le=%s holds %s samples, want 0", name, below, got)
		}
	}
	if got := count(upper); got != "1" {
		t.Errorf("%s le=%s holds %s samples, want 1", name, upper, got)
	}
}

func grep(body, substr string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
