package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/podtoken"
	"github.com/matty-v/kyber/pkg/requeststore"
	"github.com/matty-v/kyber/pkg/telemetry"
)

func newRequestReplyServer(t *testing.T, store requeststore.Store) *httptest.Server {
	t.Helper()
	server := api.NewInternalServer(
		briefstore.NewMemoryStore(),
		api.WithRequestStore(store),
		api.WithInternalAuth(api.NewHMACInternalAuthenticator(testSigningKey), false),
	)
	testServer := httptest.NewServer(server.Handler())
	t.Cleanup(testServer.Close)
	return testServer
}

func requestReply(t *testing.T, server *httptest.Server, agent, token, requestID, response string) *http.Response {
	t.Helper()
	body := `{"request_id":"` + requestID + `","response":"` + response + `"}`
	return do(t, server, http.MethodPost, "/internal/agents/"+agent+"/request-reply", token, body)
}

func dispatchedRequest(t *testing.T, store requeststore.Store, agent, id string) {
	t.Helper()
	if _, err := store.Create(context.Background(), agent, id, "prompt", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDispatched(context.Background(), agent, id); err != nil {
		t.Fatal(err)
	}
}

func TestInternalRequestReply_SingleAssignmentAndIdempotency(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter("request-reply-test")
	oldCounter, oldDuration := telemetry.AgentRequestsTotal, telemetry.AgentRequestDuration
	telemetry.AgentRequestsTotal, _ = meter.Int64Counter("kyber_agent_requests_total")
	telemetry.AgentRequestDuration, _ = meter.Float64Histogram("kyber_agent_request_duration_seconds")
	t.Cleanup(func() {
		telemetry.AgentRequestsTotal, telemetry.AgentRequestDuration = oldCounter, oldDuration
		_ = provider.Shutdown(context.Background())
	})

	store, _ := requeststore.NewMemoryStore(requeststore.DefaultLimits())
	dispatchedRequest(t, store, "alice", "req_00000000000000000000000000000001")
	server := newRequestReplyServer(t, store)
	token := podtoken.Sign("alice", testSigningKey)

	for _, response := range []string{"answer", "answer"} {
		result := requestReply(t, server, "alice", token, "req_00000000000000000000000000000001", response)
		result.Body.Close()
		if result.StatusCode != http.StatusNoContent {
			t.Fatalf("identical response status = %d", result.StatusCode)
		}
	}
	conflict := requestReply(t, server, "alice", token, "req_00000000000000000000000000000001", "different")
	defer conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting response status = %d", conflict.StatusCode)
	}

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	var completions int64
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "kyber_agent_requests_total" {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("completion metric type = %T", metric.Data)
			}
			for _, point := range sum.DataPoints {
				completions += point.Value
			}
		}
	}
	if completions != 1 {
		t.Fatalf("completion metric = %d, want 1", completions)
	}
}

func TestInternalRequestReply_CrossAgentAndStateGuards(t *testing.T) {
	store, _ := requeststore.NewMemoryStore(requeststore.DefaultLimits())
	dispatchedRequest(t, store, "alice", "req_00000000000000000000000000000002")
	if _, err := store.Create(context.Background(), "alice", "req_00000000000000000000000000000003", "prompt", ""); err != nil {
		t.Fatal(err)
	}
	server := newRequestReplyServer(t, store)

	cross := requestReply(t, server, "alice", podtoken.Sign("bob", testSigningKey), "req_00000000000000000000000000000002", "stolen")
	cross.Body.Close()
	if cross.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-agent response status = %d", cross.StatusCode)
	}
	queued := requestReply(t, server, "alice", podtoken.Sign("alice", testSigningKey), "req_00000000000000000000000000000003", "early")
	queued.Body.Close()
	if queued.StatusCode != http.StatusConflict {
		t.Fatalf("queued response status = %d", queued.StatusCode)
	}
	stored, err := store.Get(context.Background(), "alice", "req_00000000000000000000000000000002")
	if err != nil || stored.Status != requeststore.StatusDispatched || stored.Response != "" {
		t.Fatalf("cross-agent attempt mutated request: %+v, %v", stored, err)
	}
}

func TestInternalRequestReply_LateAndOversizeResponses(t *testing.T) {
	limits := requeststore.DefaultLimits()
	limits.Lifetime = 20 * time.Millisecond
	limits.MaxResponseBytes = 4
	store, _ := requeststore.NewMemoryStore(limits)
	dispatchedRequest(t, store, "alice", "req_00000000000000000000000000000004")
	dispatchedRequest(t, store, "alice", "req_00000000000000000000000000000005")
	server := newRequestReplyServer(t, store)
	token := podtoken.Sign("alice", testSigningKey)

	oversize := requestReply(t, server, "alice", token, "req_00000000000000000000000000000004", strings.Repeat("x", 5))
	oversize.Body.Close()
	if oversize.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize response status = %d", oversize.StatusCode)
	}
	time.Sleep(25 * time.Millisecond)
	late := requestReply(t, server, "alice", token, "req_00000000000000000000000000000005", "late")
	defer late.Body.Close()
	if late.StatusCode != http.StatusNotFound {
		t.Fatalf("late response status = %d", late.StatusCode)
	}
}
