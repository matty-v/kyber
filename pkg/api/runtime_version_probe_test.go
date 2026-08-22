package api_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
)

// Wire-contract round trip for the raw model-probe report fields
// (canary regression 2026-08-22): the start script reports exit+output,
// the server classifies via pkg/modelprobe, and the result must land in
// Status.Runtime — including the inconclusive case, which used to
// vanish.

func postRuntimeVersion(t *testing.T, url, body string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", resp.StatusCode)
	}
}

func probeTestRig(t *testing.T) (string, func() kyberv1.AgentRuntimeStatus) {
	t.Helper()
	scheme := newRuntimeVersionScheme(t)
	agent := newRuntimeVersionAgent("wedge")
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(agent).
		WithObjects(agent).
		Build()
	srv := api.NewInternalServer(briefstore.NewMemoryStore(),
		api.WithKubeClient(fakeClient, "kyber-system"))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	read := func() kyberv1.AgentRuntimeStatus {
		var got kyberv1.Agent
		if err := fakeClient.Get(context.Background(),
			k8stypes.NamespacedName{Name: "wedge", Namespace: "kyber-system"}, &got); err != nil {
			t.Fatalf("Get: %v", err)
		}
		return got.Status.Runtime
	}
	return ts.URL + "/internal/agents/wedge/runtime-version", read
}

func TestRuntimeVersion_ProbeRejection_ClassifiedUnsupported(t *testing.T) {
	url, read := probeTestRig(t)
	postRuntimeVersion(t, url, `{"version":"2.1.240","modelProbeExit":1,
		"modelProbeOutput":"There's an issue with the selected model (claude-opus-4-canary-marker). It may not exist or you may not have access to it."}`)
	rs := read()
	if rs.ModelSupported == nil || *rs.ModelSupported {
		t.Fatalf("ModelSupported = %v, want false", rs.ModelSupported)
	}
	if rs.ModelProbeMessage == "" {
		t.Error("ModelProbeMessage should carry the rejection text")
	}
}

func TestRuntimeVersion_ProbeInconclusive_KeepsDiagnostic(t *testing.T) {
	url, read := probeTestRig(t)
	postRuntimeVersion(t, url, `{"version":"2.1.240","modelProbeExit":1,
		"modelProbeOutput":"Invalid bearer token. Please run /login."}`)
	rs := read()
	if rs.ModelSupported != nil {
		t.Fatalf("ModelSupported = %v, want nil (inconclusive)", *rs.ModelSupported)
	}
	if rs.ModelProbeMessage == "" {
		t.Error("ModelProbeMessage must survive an inconclusive probe — silence is the bug this closes")
	}
}

func TestRuntimeVersion_ProbeSuccess_ClearsMessage(t *testing.T) {
	url, read := probeTestRig(t)
	// A prior boot left a failure recorded; a healthy boot must clear it.
	postRuntimeVersion(t, url, `{"version":"2.1.240","modelProbeExit":1,
		"modelProbeOutput":"no such model: claude-x"}`)
	postRuntimeVersion(t, url, `{"version":"2.1.240","modelProbeExit":0,"modelProbeOutput":""}`)
	rs := read()
	if rs.ModelSupported == nil || !*rs.ModelSupported {
		t.Fatalf("ModelSupported = %v, want true", rs.ModelSupported)
	}
	if rs.ModelProbeMessage != "" {
		t.Errorf("ModelProbeMessage = %q, want cleared on success", rs.ModelProbeMessage)
	}
}

func TestRuntimeVersion_LegacyBoolStillHonored(t *testing.T) {
	// Old images report only the boolean — must keep working through a
	// staggered image roll.
	url, read := probeTestRig(t)
	postRuntimeVersion(t, url, `{"version":"2.1.200","modelSupported":false}`)
	rs := read()
	if rs.ModelSupported == nil || *rs.ModelSupported {
		t.Fatalf("ModelSupported = %v, want false via legacy field", rs.ModelSupported)
	}
}

func TestRuntimeVersion_RawFieldsTakePrecedenceOverLegacyBool(t *testing.T) {
	// A reporter sending both: classification from raw wins (exit 0
	// beats a stale legacy false).
	url, read := probeTestRig(t)
	postRuntimeVersion(t, url, `{"version":"2.1.240","modelSupported":false,"modelProbeExit":0,"modelProbeOutput":""}`)
	rs := read()
	if rs.ModelSupported == nil || !*rs.ModelSupported {
		t.Fatalf("ModelSupported = %v, want true (raw fields win)", rs.ModelSupported)
	}
}
