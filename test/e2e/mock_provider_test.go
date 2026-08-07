//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestMockProvider_EndToEnd exercises the Phase B standalone-shape flow:
// Machine with provider=mock, capacity-aware Agent admission, and the
// one-per-cluster constraint on mock Machines.
func TestMockProvider_EndToEnd(t *testing.T) {
	c := newAPIClient(t)
	ctx := context.Background()

	const machineName = "local"

	// --- 1. Create a mock Machine with capacity 2 cpu / 4Gi ---
	machineBody := map[string]interface{}{
		"name":     machineName,
		"provider": "mock",
		"capacity": map[string]interface{}{
			"cpu":    "2",
			"memory": "4Gi",
		},
	}
	code, respBody, err := c.PostRaw("/api/v1/machines", machineBody)
	requireNoErr(t, err)
	if code != http.StatusCreated {
		t.Fatalf("create mock machine: status=%d body=%s", code, respBody)
	}
	t.Logf("mock machine created: %s", respBody)

	// Register cleanup before any failure point so it always runs.
	t.Cleanup(func() {
		_, _ = c.Delete("/api/v1/agents/small")
		_, _ = c.Delete("/api/v1/machines/" + machineName)
	})

	// --- 2. Wait for Status.Phase=Ready with NodeName set ---
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	err = WaitForCondition(waitCtx, 60*time.Second, "machine local phase=Ready with nodeName", func() (bool, error) {
		statusCode, body, getErr := c.GetRaw("/api/v1/machines/" + machineName)
		if getErr != nil {
			return false, getErr
		}
		if statusCode != http.StatusOK {
			return false, nil
		}
		bodyStr := string(body)
		return strings.Contains(bodyStr, `"Ready"`) &&
			strings.Contains(bodyStr, `"nodeName"`), nil
	})
	if err != nil {
		_, finalBody, _ := c.GetRaw("/api/v1/machines/" + machineName)
		t.Fatalf("mock machine did not reach Ready within 60s: %s (wait error: %v)", finalBody, err)
	}

	// Verify the final state explicitly.
	_, finalBody, err := c.GetRaw("/api/v1/machines/" + machineName)
	requireNoErr(t, err)
	finalStr := string(finalBody)
	if !strings.Contains(finalStr, `"Ready"`) {
		t.Fatalf("mock machine phase not Ready in final GET: %s", finalStr)
	}
	if !strings.Contains(finalStr, `"nodeName"`) {
		t.Fatalf("mock machine reached Ready but nodeName not populated: %s", finalStr)
	}
	t.Logf("mock machine ready: %s", finalStr)

	// --- 3. Attempt a second mock Machine → 409 with "one Machine" ---
	secondBody := map[string]interface{}{
		"name":     "local-2",
		"provider": "mock",
		"capacity": map[string]interface{}{
			"cpu":    "1",
			"memory": "2Gi",
		},
	}
	code2, body2, err := c.PostRaw("/api/v1/machines", secondBody)
	requireNoErr(t, err)
	if code2 != http.StatusConflict {
		t.Fatalf("second mock machine: status=%d want 409; body=%s", code2, body2)
	}
	if !strings.Contains(string(body2), "one Machine") {
		t.Errorf("second mock machine error should mention 'one Machine'; got %s", body2)
	}
	t.Logf("second mock machine correctly rejected: %s", body2)

	// --- 4. Agent that fits → 201 ---
	smallAgent := map[string]interface{}{
		"name":    "small",
		"machine": machineName,
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu":    "500m",
			"memory": "512Mi",
		},
		"secrets": map[string]interface{}{
			"oauthToken": "dummy-token-for-e2e",
		},
	}
	codeA, bodyA, err := c.PostRaw("/api/v1/agents", smallAgent)
	requireNoErr(t, err)
	if codeA != http.StatusCreated {
		t.Fatalf("create small agent: status=%d body=%s", codeA, bodyA)
	}
	t.Logf("small agent created: %s", bodyA)

	// --- 5. Agent that overflows → 409 INSUFFICIENT_CAPACITY ---
	bigAgent := map[string]interface{}{
		"name":    "big",
		"machine": machineName,
		"runtime": "claude-code",
		"model":   "claude-sonnet-4",
		"resources": map[string]interface{}{
			"cpu":    "4",
			"memory": "8Gi",
		},
		"secrets": map[string]interface{}{
			"oauthToken": "dummy-token-for-e2e",
		},
	}
	codeB, bodyB, err := c.PostRaw("/api/v1/agents", bigAgent)
	requireNoErr(t, err)
	if codeB != http.StatusConflict {
		t.Fatalf("overflow agent: status=%d want 409; body=%s", codeB, bodyB)
	}
	bodyBStr := string(bodyB)
	if !strings.Contains(bodyBStr, "insufficient capacity") &&
		!strings.Contains(bodyBStr, "INSUFFICIENT_CAPACITY") {
		t.Errorf("overflow agent error should mention capacity; got %s", bodyBStr)
	}
	t.Logf("overflow agent correctly rejected: %s", bodyBStr)
}
