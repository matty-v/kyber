package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/podtoken"
	"github.com/matty-v/kyber/pkg/tokenreport"
	"github.com/matty-v/kyber/pkg/usagestore"
)

type usageStoreSpy struct {
	events []usagestore.Event
	gaps   []usagestore.Gap
	last   usagestore.SourceCursor
}

func (s *usageStoreSpy) ResolveRoute(_ context.Context, uid string, route usagestore.RouteRevision) (string, error) {
	if uid == "" || route.Runtime != "claude" {
		return "", usagestore.ErrInvalid
	}
	return uuid.NewString(), nil
}

func (s *usageStoreSpy) IngestWithCursor(_ context.Context, event usagestore.Event, cursor *usagestore.SourceCursor) (usagestore.Event, bool, error) {
	if err := event.Validate(); err != nil {
		return usagestore.Event{}, false, err
	}
	if err := cursor.Validate(); err != nil {
		return usagestore.Event{}, false, err
	}
	s.events = append(s.events, event)
	s.last = *cursor
	return event, true, nil
}

func (s *usageStoreSpy) RecordGapWithCursor(_ context.Context, gap usagestore.Gap, cursor *usagestore.SourceCursor) error {
	if err := gap.Validate(); err != nil {
		return err
	}
	s.gaps = append(s.gaps, gap)
	s.last = *cursor
	return nil
}

func TestInternalUsage_AuthenticatedLifetimeRouteAndAllowlist(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agentUID := uuid.NewString()
	agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "kyber-system", UID: types.UID(agentUID)},
		Spec: kyberv1.AgentSpec{Runtime: "claude-code"}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	readerClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent.DeepCopy()).Build()
	spy := &usageStoreSpy{}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	server := api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(client, "kyber-system"),
		api.WithUsageAgentReader(readerClient),
		api.WithUsageStore(spy), api.WithInternalAuth(api.NewHMACInternalAuthenticator(key), false))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	route, err := usagestore.RouteRevisionFor("claude-code", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := route.FingerprintHex()
	if err != nil {
		t.Fatal(err)
	}
	cursor := tokenreport.UsageAckCursor{Namespace: uuid.NewString(), Offset: 120}
	request := tokenreport.UsageEventRequest{AgentUID: agentUID, RouteFingerprint: fingerprint, Cursor: cursor,
		Evidence: tokenreport.UsageEvidence{Runtime: "claude", SourceIdentity: cursor.Namespace + ":msg_1",
			ObservedAt: time.Now().UTC(), Model: "claude-test", ModelEvidence: "observed", InputUncached: func() *int64 { n := int64(50); return &n }()}}
	post := func(path, token string, body any) int {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	aliceToken := podtoken.Sign("alice", key)
	if got := post("/internal/agents/alice/usage-event", podtoken.Sign("bob", key), request); got != http.StatusForbidden {
		t.Fatalf("cross-agent status=%d", got)
	}
	if got := post("/internal/agents/alice/usage-event", "", request); got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", got)
	}
	badUID := request
	badUID.AgentUID = uuid.NewString()
	if got := post("/internal/agents/alice/usage-event", aliceToken, badUID); got != http.StatusConflict {
		t.Fatalf("stale lifetime status=%d", got)
	}
	badRoute := request
	badRoute.RouteFingerprint = strings.Repeat("0", 64)
	if got := post("/internal/agents/alice/usage-event", aliceToken, badRoute); got != http.StatusConflict {
		t.Fatalf("stale route status=%d", got)
	}
	if got := post("/internal/agents/alice/usage-event", aliceToken, map[string]any{"agentUID": agentUID, "routeFingerprint": fingerprint, "evidence": request.Evidence, "cursor": cursor, "prompt": "must reject"}); got != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d", got)
	}
	if got := post("/internal/agents/alice/usage-event", aliceToken, request); got != http.StatusNoContent {
		t.Fatalf("valid event status=%d", got)
	}
	if len(spy.events) != 1 || spy.events[0].AgentUID != agentUID || spy.events[0].RouteID == "" || spy.last != (usagestore.SourceCursor{Namespace: cursor.Namespace, Offset: cursor.Offset}) {
		t.Fatalf("event=%+v cursor=%+v", spy.events, spy.last)
	}
	gap := tokenreport.UsageGapRequest{AgentUID: agentUID, RouteFingerprint: fingerprint, Runtime: "claude",
		SourceIdentity: cursor.Namespace + ":malformed-120", Reason: "malformed", DetectedAt: time.Now().UTC(), Cursor: cursor}
	if got := post("/internal/agents/alice/usage-gap", aliceToken, gap); got != http.StatusNoContent || len(spy.gaps) != 1 {
		t.Fatalf("gap status=%d gaps=%+v", got, spy.gaps)
	}
	// The cached client still holds the old Agent lifetime. A direct API
	// reader seeing a replacement must reject the old pod's next write.
	replaced := agent.DeepCopy()
	replaced.UID = types.UID(uuid.NewString())
	if err := readerClient.Update(context.Background(), replaced); err != nil {
		t.Fatal(err)
	}
	if got := post("/internal/agents/alice/usage-event", aliceToken, request); got != http.StatusConflict {
		t.Fatalf("stale cache accepted old lifetime: %d", got)
	}
}
