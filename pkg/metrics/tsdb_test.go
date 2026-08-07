package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestQueryInstantAt_ForwardsEvalTime verifies that QueryInstantAt anchors the
// instant query at the requested evaluation time by sending Prometheus the
// `time` parameter (kyber#428 — Tier-2 token fallback must evaluate increase()
// at the window end, not at "now"), while QueryInstant sends no `time` param.
func TestQueryInstantAt_ForwardsEvalTime(t *testing.T) {
	var gotTime string
	var sawTimeParam bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotTime = q.Get("time")
		_, sawTimeParam = q["time"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	c := NewTSDBClient(Config{PrometheusURL: srv.URL})
	ctx := context.Background()

	// Evaluated at an explicit end timestamp → `time` param present and exact.
	if _, err := c.QueryInstantAt(ctx, "up", 1700000000); err != nil {
		t.Fatalf("QueryInstantAt: %v", err)
	}
	if gotTime != "1700000000" {
		t.Errorf("time param = %q, want 1700000000", gotTime)
	}

	// evalUnix <= 0 → no `time` param (evaluate at server now).
	sawTimeParam = false
	if _, err := c.QueryInstantAt(ctx, "up", 0); err != nil {
		t.Fatalf("QueryInstantAt(0): %v", err)
	}
	if sawTimeParam {
		t.Errorf("evalUnix=0 must not send a time param")
	}

	// QueryInstant delegates with no eval time → no `time` param.
	sawTimeParam = false
	if _, err := c.QueryInstant(ctx, "up"); err != nil {
		t.Fatalf("QueryInstant: %v", err)
	}
	if sawTimeParam {
		t.Errorf("QueryInstant must not send a time param")
	}
}
