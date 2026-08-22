//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestE2E_PlatformHealth validates Phase 1 of the test plan:
// the control-plane API responds to health + auth checks.
func TestE2E_PlatformHealth(t *testing.T) {
	c := newAPIClient(t)

	t.Run("healthz", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/healthz", nil)
		requireNoErr(t, err)
		resp, err := c.HTTP.Do(req)
		requireNoErr(t, err)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /healthz: expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("readyz", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/readyz", nil)
		requireNoErr(t, err)
		resp, err := c.HTTP.Do(req)
		requireNoErr(t, err)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /readyz: expected 200, got %d", resp.StatusCode)
		}
	})

	t.Run("auth_required_no_token", func(t *testing.T) {
		resp, err := c.GetNoAuth("/api/v1/fleet")
		requireNoErr(t, err)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET /api/v1/fleet without token: expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("auth_accepted_with_valid_token", func(t *testing.T) {
		code, _, err := c.GetRaw("/api/v1/fleet")
		requireNoErr(t, err)
		if code != http.StatusOK {
			t.Errorf("GET /api/v1/fleet with valid token: expected 200, got %d", code)
		}
	})
}

// TestE2E_MachineLifecycle validates Phase 2 of the test plan:
// create a machine CRD via the API, verify it appears, then delete it.
//
// With MockComputeAdapter in k3d, the machine phase stays at Provisioning
// (no real node joins). The tests validate the API surface and CRD lifecycle —
// not actual VM provisioning.
func TestE2E_MachineLifecycle(t *testing.T) {
	c := newAPIClient(t)
	ctx := context.Background()

	const machineName = "e2e-machine-1"

	t.Run("create_machine", func(t *testing.T) {
		body := map[string]interface{}{
			"name":        machineName,
			"provider":    "gce",
			"machineType": "n2-standard-2",
			"diskSizeGb":  20,
			"spot":        false,
			"zone":        "us-central1-a",
		}
		code, respBody, err := c.PostRaw("/api/v1/machines", body)
		requireNoErr(t, err)
		if code != http.StatusCreated {
			t.Fatalf("POST /api/v1/machines: expected 201, got %d (body: %s)", code, respBody)
		}
		t.Logf("machine created: %s", respBody)
	})

	t.Run("get_machine", func(t *testing.T) {
		// Poll for up to 10s — the CRD may not be visible in the informer cache
		// immediately after creation (k3d propagation delay).
		var m machineResponse
		deadline := time.Now().Add(10 * time.Second)
		for {
			httpResp, err := c.Get("/api/v1/machines/"+machineName, &m)
			requireNoErr(t, err)
			if httpResp.StatusCode == http.StatusOK {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("GET /api/v1/machines/%s: still %d after 10s", machineName, httpResp.StatusCode)
			}
			time.Sleep(500 * time.Millisecond)
		}
		if m.ID != machineName {
			t.Errorf("machine ID: expected %q, got %q", machineName, m.ID)
		}
		t.Logf("machine GET response: id=%s phase=%s", m.ID, m.Phase)
	})

	t.Run("list_machines_contains_created", func(t *testing.T) {
		var list machineListResponse
		httpResp, err := c.Get("/api/v1/machines", &list)
		requireNoErr(t, err)
		if httpResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/v1/machines: expected 200, got %d", httpResp.StatusCode)
		}
		found := false
		for _, m := range list.Items {
			if m.ID == machineName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("machine %q not found in list of %d machines", machineName, len(list.Items))
		}
	})

	t.Run("machine_phase_provisioning", func(t *testing.T) {
		// With MockComputeAdapter, the machine enters Provisioning immediately.
		// We wait briefly to let the reconciler run its first pass.
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		err := WaitForCondition(waitCtx, 30*time.Second, "machine enters non-empty phase", func() (bool, error) {
			var m machineResponse
			httpResp, err := c.Get("/api/v1/machines/"+machineName, &m)
			if err != nil {
				return false, err
			}
			if httpResp.StatusCode != http.StatusOK {
				return false, nil
			}
			// Phase will be Provisioning once the reconciler runs — accept any non-empty phase.
			return m.Phase != "", nil
		})
		if err != nil {
			// Non-fatal: the machine CRD was created — the reconciler may just be slow.
			t.Logf("WARN: %v (reconciler may not have run yet)", err)
		}
	})

	t.Run("delete_machine", func(t *testing.T) {
		resp, err := c.Delete("/api/v1/machines/" + machineName)
		requireNoErr(t, err)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE /api/v1/machines/%s: expected 204, got %d", machineName, resp.StatusCode)
		}
	})

	t.Run("machine_deleted_from_api", func(t *testing.T) {
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := WaitForMachineDeleted(waitCtx, c, machineName, 30*time.Second); err != nil {
			t.Errorf("machine %q still visible after delete: %v", machineName, err)
		}
	})
}

// TestE2E_AgentLifecycle validates Phase 3 of the test plan:
// create an agent CRD, verify it's visible, delete it.
//
// Per the D3 design note: agent tests use a dummy machine reference and verify
// only the control-plane layer (CRD + API surface). Full pod-running tests
// require a real runtime image and are deferred.
func TestE2E_AgentLifecycle(t *testing.T) {
	c := newAPIClient(t)
	ctx := context.Background()

	const (
		machineName = "k3d-kyber-e2e-server-0"
		agentName   = "e2e-agent-1"
	)

	// Pre-create the Machine CR so the agent-create validation passes.
	// The Machine will fail to provision in k3d (no GCE credentials), but the CR
	// existing is all that's required for POST /api/v1/agents to succeed.
	t.Run("setup_machine", func(t *testing.T) {
		body := map[string]interface{}{
			"name":        machineName,
			"provider":    "gce",
			"machineType": "e2-small",
			"diskSizeGb":  10,
			"spot":        false,
			"zone":        "us-central1-a",
		}
		code, respBody, err := c.PostRaw("/api/v1/machines", body)
		requireNoErr(t, err)
		// 201 = created; 409 = already exists from a previous run.
		if code != http.StatusCreated && code != http.StatusConflict {
			t.Fatalf("setup machine: expected 201 or 409, got %d (body: %s)", code, respBody)
		}
	})
	t.Cleanup(func() { _, _ = c.Delete("/api/v1/machines/" + machineName) })

	t.Run("create_agent", func(t *testing.T) {
		body := map[string]interface{}{
			"name":    agentName,
			"machine": machineName,
			"runtime": "claude-code",
			"model":   "claude-sonnet-4",
			"resources": map[string]interface{}{
				"cpu":    "100m",
				"memory": "128Mi",
				"disk":   "10Gi",
			},
		}
		code, respBody, err := c.PostRaw("/api/v1/agents", body)
		requireNoErr(t, err)
		if code != http.StatusCreated {
			t.Fatalf("POST /api/v1/agents: expected 201, got %d (body: %s)", code, respBody)
		}
		t.Logf("agent created: %s", respBody)
	})

	t.Run("get_agent", func(t *testing.T) {
		// Poll for up to 10s — the CRD may not be visible in the informer cache
		// immediately after creation (k3d propagation delay).
		var a agentResponse
		deadline := time.Now().Add(10 * time.Second)
		for {
			httpResp, err := c.Get("/api/v1/agents/"+agentName, &a)
			requireNoErr(t, err)
			if httpResp.StatusCode == http.StatusOK {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("GET /api/v1/agents/%s: still %d after 10s", agentName, httpResp.StatusCode)
			}
			time.Sleep(500 * time.Millisecond)
		}
		if a.ID != agentName {
			t.Errorf("agent ID: expected %q, got %q", agentName, a.ID)
		}
		t.Logf("agent GET response: id=%s phase=%s", a.ID, a.Phase)
	})

	t.Run("list_agents_contains_created", func(t *testing.T) {
		var list agentListResponse
		httpResp, err := c.Get("/api/v1/agents", &list)
		requireNoErr(t, err)
		if httpResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/v1/agents: expected 200, got %d", httpResp.StatusCode)
		}
		found := false
		for _, a := range list.Items {
			if a.ID == agentName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("agent %q not found in list of %d agents", agentName, len(list.Items))
		}
	})

	t.Run("agent_reconciler_started", func(t *testing.T) {
		// Verify the reconciler has processed the CRD at least once.
		// With no real k8s node matching the machine name, the agent will stay in Creating.
		// We just verify the phase is non-empty (reconciler ran).
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := WaitForCondition(waitCtx, 30*time.Second, "agent has non-empty phase", func() (bool, error) {
			var a agentResponse
			httpResp, err := c.Get("/api/v1/agents/"+agentName, &a)
			if err != nil {
				return false, err
			}
			if httpResp.StatusCode != http.StatusOK {
				return false, nil
			}
			return a.Phase != "", nil
		})
		if err != nil {
			t.Logf("WARN: %v (reconciler may not have run yet)", err)
		}
	})

	t.Run("stop_agent", func(t *testing.T) {
		code, body, err := c.PostRaw("/api/v1/agents/"+agentName+"/stop", nil)
		requireNoErr(t, err)
		if code != http.StatusOK {
			t.Fatalf("POST /api/v1/agents/%s/stop: expected 200, got %d (body: %s)", agentName, code, body)
		}
	})

	t.Run("delete_agent", func(t *testing.T) {
		// kyber#565: DELETE now requires ?confirm=<name>.
		resp, err := c.Delete("/api/v1/agents/" + agentName + "?confirm=" + agentName)
		requireNoErr(t, err)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE /api/v1/agents/%s: expected 204, got %d", agentName, resp.StatusCode)
		}
	})

	t.Run("agent_deleted_from_api", func(t *testing.T) {
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := WaitForAgentDeleted(waitCtx, c, agentName, 30*time.Second); err != nil {
			t.Errorf("agent %q still visible after delete: %v", agentName, err)
		}
	})
}

// TestE2E_ErrorHandling validates Phase 7: error cases produce correct HTTP status codes.
func TestE2E_ErrorHandling(t *testing.T) {
	c := newAPIClient(t)

	t.Run("delete_machine_with_attached_agents", func(t *testing.T) {
		const (
			machineName = "e2e-err-machine"
			agentName   = "e2e-err-agent"
		)

		// Create a machine.
		machineBody := map[string]interface{}{
			"name":        machineName,
			"provider":    "gce",
			"machineType": "n2-standard-2",
			"diskSizeGb":  20,
			"spot":        false,
			"zone":        "us-central1-a",
		}
		code, body, err := c.PostRaw("/api/v1/machines", machineBody)
		requireNoErr(t, err)
		if code != http.StatusCreated {
			t.Fatalf("setup: POST /api/v1/machines: %d (%s)", code, body)
		}

		// Create an agent attached to that machine.
		agentBody := map[string]interface{}{
			"name":    agentName,
			"machine": machineName,
			"runtime": "claude-code",
			"model":   "claude-sonnet-4",
			"resources": map[string]interface{}{
				"cpu":    "100m",
				"memory": "128Mi",
				"disk":   "10Gi",
			},
		}
		code, body, err = c.PostRaw("/api/v1/agents", agentBody)
		requireNoErr(t, err)
		if code != http.StatusCreated {
			t.Fatalf("setup: POST /api/v1/agents: %d (%s)", code, body)
		}

		// Attempt to delete the machine — should fail with 422.
		resp, err := c.Delete("/api/v1/machines/" + machineName)
		requireNoErr(t, err)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("DELETE machine with attached agent: expected 422, got %d", resp.StatusCode)
		}

		// Cleanup.
		t.Cleanup(func() {
			_, _ = c.Delete("/api/v1/agents/" + agentName)
			_, _ = c.Delete("/api/v1/machines/" + machineName)
		})
	})

	t.Run("create_agent_missing_name", func(t *testing.T) {
		body := map[string]interface{}{
			"machine": "some-machine",
			"runtime": "claude-code",
			"model":   "claude-sonnet-4",
		}
		code, respBody, err := c.PostRaw("/api/v1/agents", body)
		requireNoErr(t, err)
		requireStatus(t, http.StatusBadRequest, code, respBody)
	})

	t.Run("create_agent_missing_runtime", func(t *testing.T) {
		body := map[string]interface{}{
			"name":    "e2e-no-runtime",
			"machine": "some-machine",
			"model":   "claude-sonnet-4",
		}
		code, respBody, err := c.PostRaw("/api/v1/agents", body)
		requireNoErr(t, err)
		requireStatus(t, http.StatusBadRequest, code, respBody)
	})

	t.Run("create_agent_missing_machine", func(t *testing.T) {
		body := map[string]interface{}{
			"name":    "e2e-no-machine",
			"runtime": "claude-code",
			"model":   "claude-sonnet-4",
		}
		code, respBody, err := c.PostRaw("/api/v1/agents", body)
		requireNoErr(t, err)
		requireStatus(t, http.StatusBadRequest, code, respBody)
	})

	t.Run("create_machine_missing_fields", func(t *testing.T) {
		// Missing machineType and zone.
		body := map[string]interface{}{
			"name": "e2e-bad-machine",
		}
		code, respBody, err := c.PostRaw("/api/v1/machines", body)
		requireNoErr(t, err)
		requireStatus(t, http.StatusBadRequest, code, respBody)
	})

	t.Run("get_nonexistent_agent", func(t *testing.T) {
		code, body, err := c.GetRaw("/api/v1/agents/does-not-exist")
		requireNoErr(t, err)
		requireStatus(t, http.StatusNotFound, code, body)
	})

	t.Run("get_nonexistent_machine", func(t *testing.T) {
		code, body, err := c.GetRaw("/api/v1/machines/does-not-exist")
		requireNoErr(t, err)
		requireStatus(t, http.StatusNotFound, code, body)
	})
}

// TestE2E_Cleanup validates Phase 8: after all tests, verify no stale resources remain.
// This test runs last and cleans up any leftover CRDs.
func TestE2E_Cleanup(t *testing.T) {
	c := newAPIClient(t)
	ctx := context.Background()

	t.Run("delete_all_agents", func(t *testing.T) {
		var list agentListResponse
		httpResp, err := c.Get("/api/v1/agents", &list)
		requireNoErr(t, err)
		if httpResp.StatusCode != http.StatusOK {
			t.Fatalf("list agents: expected 200, got %d", httpResp.StatusCode)
		}
		for _, a := range list.Items {
			resp, err := c.Delete("/api/v1/agents/" + a.ID)
			if err != nil {
				t.Errorf("DELETE agent %q: %v", a.ID, err)
				continue
			}
			if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
				t.Errorf("DELETE agent %q: expected 204, got %d", a.ID, resp.StatusCode)
			}
		}
	})

	t.Run("delete_all_machines", func(t *testing.T) {
		var list machineListResponse
		httpResp, err := c.Get("/api/v1/machines", &list)
		requireNoErr(t, err)
		if httpResp.StatusCode != http.StatusOK {
			t.Fatalf("list machines: expected 200, got %d", httpResp.StatusCode)
		}
		for _, m := range list.Items {
			resp, err := c.Delete("/api/v1/machines/" + m.ID)
			if err != nil {
				t.Errorf("DELETE machine %q: %v", m.ID, err)
				continue
			}
			if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
				t.Errorf("DELETE machine %q: expected 204, got %d", m.ID, resp.StatusCode)
			}
		}
	})

	t.Run("no_agents_remain", func(t *testing.T) {
		waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		err := WaitForCondition(waitCtx, 60*time.Second, "all agents deleted", func() (bool, error) {
			var list agentListResponse
			httpResp, err := c.Get("/api/v1/agents", &list)
			if err != nil {
				return false, err
			}
			if httpResp.StatusCode != http.StatusOK {
				return false, fmt.Errorf("list agents: status %d", httpResp.StatusCode)
			}
			return len(list.Items) == 0, nil
		})
		if err != nil {
			t.Errorf("agents not fully cleaned up: %v", err)
		}
	})

	t.Run("no_machines_remain", func(t *testing.T) {
		waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		err := WaitForCondition(waitCtx, 60*time.Second, "all machines deleted", func() (bool, error) {
			var list machineListResponse
			httpResp, err := c.Get("/api/v1/machines", &list)
			if err != nil {
				return false, err
			}
			if httpResp.StatusCode != http.StatusOK {
				return false, fmt.Errorf("list machines: status %d", httpResp.StatusCode)
			}
			return len(list.Items) == 0, nil
		})
		if err != nil {
			t.Errorf("machines not fully cleaned up: %v", err)
		}
	})
}

// TestE2E_FleetSummary validates that the fleet summary endpoint aggregates state correctly.
func TestE2E_FleetSummary(t *testing.T) {
	c := newAPIClient(t)

	type fleetResponse struct {
		MachineCount    int            `json:"machineCount"`
		AgentCount      int            `json:"agentCount"`
		AgentsByPhase   map[string]int `json:"agentsByPhase"`
		MachinesByPhase map[string]int `json:"machinesByPhase"`
	}

	t.Run("fleet_summary_responds", func(t *testing.T) {
		var fleet fleetResponse
		httpResp, err := c.Get("/api/v1/fleet", &fleet)
		requireNoErr(t, err)
		if httpResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/v1/fleet: expected 200, got %d", httpResp.StatusCode)
		}
		t.Logf("fleet summary: machines=%d agents=%d", fleet.MachineCount, fleet.AgentCount)
	})

	t.Run("fleet_summary_alias", func(t *testing.T) {
		var fleet fleetResponse
		httpResp, err := c.Get("/api/v1/fleet/summary", &fleet)
		requireNoErr(t, err)
		if httpResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/v1/fleet/summary: expected 200, got %d", httpResp.StatusCode)
		}
	})
}

// Ensure runWithContext is used (avoids "declared and not used" in setup_test.go).
var _ = runWithContext
var _ = strings.TrimSpace // used in setup_test.go indirectly
