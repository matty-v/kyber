# Preemption Resilience Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Kyber agents survive spot VM preemptions with durable disk, graceful drain, and preemption-aware restart logic.

**Architecture:** Six components wired together: regional PD storage for durability, node agent preemption detector as the trigger, control plane drain API as the coordinator, agent state machine changes for the response, enriched session briefs for context handoff, and runtime notifications for user transparency.

**Tech Stack:** Go, k8s controller-runtime, GCE PD CSI driver, GCE metadata API, Helm

**Spec:** `docs/2026-04-13-preemption-resilience-design.md`

---

### Task 1: Brief Store — Add PreemptionContext and New Types

**Files:**
- Modify: `pkg/briefstore/store.go` (Brief struct, lines 30-65)
- Test: `pkg/briefstore/store_test.go`

- [ ] **Step 1: Write failing test for PreemptionContext serialization**

```go
// pkg/briefstore/store_test.go
func TestBriefPreemptionContext(t *testing.T) {
	b := Brief{
		Version:       1,
		AgentName:     "test-agent",
		Timestamp:     "2026-04-13T12:00:00Z",
		ShutdownType:  ShutdownTypePreemption,
		RestartReason: RestartReasonPreemption,
		Metadata: BriefMetadata{
			PreviousModel: "sonnet",
			UptimeSeconds: 3600,
			RestartCount:  0,
			PreemptionContext: &PreemptionContext{
				InstanceId:    "kyber-testvm",
				Zone:          "us-central1-a",
				Timestamp:     "2026-04-13T11:59:30Z",
				GracefulDrain: true,
			},
		},
	}

	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Brief
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.ShutdownType != ShutdownTypePreemption {
		t.Errorf("ShutdownType: got %q, want %q", got.ShutdownType, ShutdownTypePreemption)
	}
	if got.RestartReason != RestartReasonPreemption {
		t.Errorf("RestartReason: got %q, want %q", got.RestartReason, RestartReasonPreemption)
	}
	if got.Metadata.PreemptionContext == nil {
		t.Fatal("PreemptionContext: got nil")
	}
	if got.Metadata.PreemptionContext.InstanceId != "kyber-testvm" {
		t.Errorf("InstanceId: got %q, want %q", got.Metadata.PreemptionContext.InstanceId, "kyber-testvm")
	}
	if !got.Metadata.PreemptionContext.GracefulDrain {
		t.Error("GracefulDrain: got false, want true")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/kyber && go test ./pkg/briefstore/ -run TestBriefPreemptionContext -v`
Expected: FAIL — `ShutdownTypePreemption` and `PreemptionContext` undefined

- [ ] **Step 3: Add types to Brief struct**

In `pkg/briefstore/store.go`, add constants and extend structs:

```go
const (
	ShutdownTypePlanned    = "planned"
	ShutdownTypeUnplanned  = "unplanned"
	ShutdownTypeWake       = "wake"
	ShutdownTypePreemption = "preemption"

	RestartReasonFirstBoot  = "first_boot"
	RestartReasonOperator   = "operator"
	RestartReasonCrash      = "crash"
	RestartReasonWake       = "wake"
	RestartReasonPreemption = "preemption"
)

type PreemptionContext struct {
	InstanceId    string `json:"instanceId"`
	Zone          string `json:"zone"`
	Timestamp     string `json:"timestamp"`
	GracefulDrain bool   `json:"gracefulDrain"`
}
```

Add to `BriefMetadata`:

```go
type BriefMetadata struct {
	PreviousModel     string             `json:"previousModel,omitempty"`
	UptimeSeconds     int64              `json:"uptimeSeconds,omitempty"`
	RestartCount      int32              `json:"restartCount,omitempty"`
	PreemptionContext *PreemptionContext  `json:"preemptionContext,omitempty"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/dev/kyber && go test ./pkg/briefstore/ -run TestBriefPreemptionContext -v`
Expected: PASS

- [ ] **Step 5: Run full test suite to check for regressions**

Run: `cd ~/dev/kyber && go test ./pkg/briefstore/ -v`
Expected: All tests PASS

- [ ] **Step 6: Commit**

```bash
git add pkg/briefstore/store.go pkg/briefstore/store_test.go
git commit -m "feat(briefstore): add PreemptionContext and preemption shutdown/restart types"
```

---

### Task 2: Agent CRD — Add WaitingForMachine and Draining Phases

**Files:**
- Modify: `pkg/api/v1/agent_types.go` (phase constants, lines 13-29)
- Test: `pkg/controllers/agent/state_machine_test.go`

- [ ] **Step 1: Add new phase constants**

In `pkg/api/v1/agent_types.go`, add after existing phase constants:

```go
AgentPhaseDraining          AgentPhase = "Draining"
AgentPhaseWaitingForMachine AgentPhase = "WaitingForMachine"
```

- [ ] **Step 2: Run existing tests to verify no regressions**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/agent/ -v`
Expected: All existing tests PASS (adding constants doesn't break anything)

- [ ] **Step 3: Commit**

```bash
git add pkg/api/v1/agent_types.go
git commit -m "feat(api): add Draining and WaitingForMachine agent phases"
```

---

### Task 3: Agent State Machine — Preemption Transitions

**Files:**
- Modify: `pkg/controllers/agent/state_machine.go` (events, actions, transition table)
- Test: `pkg/controllers/agent/state_machine_test.go`

- [ ] **Step 1: Write failing tests for new transitions**

```go
// pkg/controllers/agent/state_machine_test.go

func TestPreemptionTransitions(t *testing.T) {
	tests := []struct {
		name      string
		phase     kyberv1.AgentPhase
		event     Event
		wantPhase kyberv1.AgentPhase
		wantAction Action
	}{
		{
			name:       "Running + PreemptionNotice → Draining",
			phase:      kyberv1.AgentPhaseRunning,
			event:      EventPreemptionNotice,
			wantPhase:  kyberv1.AgentPhaseDraining,
			wantAction: ActionDrainAgent,
		},
		{
			name:       "Draining + PodDeleted → WaitingForMachine",
			phase:      kyberv1.AgentPhaseDraining,
			event:      EventPodDeleted,
			wantPhase:  kyberv1.AgentPhaseWaitingForMachine,
			wantAction: ActionTransitionToWaiting,
		},
		{
			name:       "Running + MachinePreempted → WaitingForMachine",
			phase:      kyberv1.AgentPhaseRunning,
			event:      EventMachinePreempted,
			wantPhase:  kyberv1.AgentPhaseWaitingForMachine,
			wantAction: ActionTransitionToWaiting,
		},
		{
			name:       "WaitingForMachine + MachineReady → Starting",
			phase:      kyberv1.AgentPhaseWaitingForMachine,
			event:      EventMachineReady,
			wantPhase:  kyberv1.AgentPhaseStarting,
			wantAction: ActionWriteBriefAndCreatePod,
		},
		{
			name:       "Starting + MachinePreempted → WaitingForMachine",
			phase:      kyberv1.AgentPhaseStarting,
			event:      EventMachinePreempted,
			wantPhase:  kyberv1.AgentPhaseWaitingForMachine,
			wantAction: ActionTransitionToWaiting,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := NextPhase(tt.phase, tt.event)
			if !ok {
				t.Fatalf("no transition for %s + %s", tt.phase, tt.event)
			}
			if result.NextPhase != tt.wantPhase {
				t.Errorf("NextPhase: got %q, want %q", result.NextPhase, tt.wantPhase)
			}
			if result.Action != tt.wantAction {
				t.Errorf("Action: got %q, want %q", result.Action, tt.wantAction)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/agent/ -run TestPreemptionTransitions -v`
Expected: FAIL — undefined events and actions

- [ ] **Step 3: Add new events, actions, and transitions**

In `pkg/controllers/agent/state_machine.go`, add event constants:

```go
EventPreemptionNotice  Event = "PreemptionNotice"
EventMachinePreempted  Event = "MachinePreempted"
EventMachineReady      Event = "MachineReady"
```

Add action constants:

```go
ActionDrainAgent          Action = "DrainAgent"
ActionTransitionToWaiting Action = "TransitionToWaiting"
```

Add transitions to the table:

```go
// Preemption: graceful drain path
{phase: kyberv1.AgentPhaseRunning, event: EventPreemptionNotice}: {
    Action:    ActionDrainAgent,
    NextPhase: kyberv1.AgentPhaseDraining,
},
// Preemption: drain complete
{phase: kyberv1.AgentPhaseDraining, event: EventPodDeleted}: {
    Action:    ActionTransitionToWaiting,
    NextPhase: kyberv1.AgentPhaseWaitingForMachine,
},
// Preemption: ungraceful (pod died before drain)
{phase: kyberv1.AgentPhaseRunning, event: EventMachinePreempted}: {
    Action:    ActionTransitionToWaiting,
    NextPhase: kyberv1.AgentPhaseWaitingForMachine,
},
// Preemption: pod was starting when machine died
{phase: kyberv1.AgentPhaseStarting, event: EventMachinePreempted}: {
    Action:    ActionTransitionToWaiting,
    NextPhase: kyberv1.AgentPhaseWaitingForMachine,
},
// Preemption: draining but node died first
{phase: kyberv1.AgentPhaseDraining, event: EventMachinePreempted}: {
    Action:    ActionTransitionToWaiting,
    NextPhase: kyberv1.AgentPhaseWaitingForMachine,
},
// Recovery: machine replaced, resume agent
{phase: kyberv1.AgentPhaseWaitingForMachine, event: EventMachineReady}: {
    Action:    ActionWriteBriefAndCreatePod,
    NextPhase: kyberv1.AgentPhaseStarting,
},
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/agent/ -run TestPreemptionTransitions -v`
Expected: PASS

- [ ] **Step 5: Run full agent controller tests**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/agent/ -v`
Expected: All tests PASS

- [ ] **Step 6: Commit**

```bash
git add pkg/controllers/agent/state_machine.go pkg/controllers/agent/state_machine_test.go
git commit -m "feat(agent): add preemption events, actions, and state transitions"
```

---

### Task 4: Agent Reconciler — Preemption-Aware Event Classification

**Files:**
- Modify: `pkg/controllers/agent/reconciler.go` (classifyEvent, action handlers)
- Test: `pkg/controllers/agent/reconciler_test.go`

- [ ] **Step 1: Write failing test for preemption classification**

```go
// pkg/controllers/agent/reconciler_test.go

func TestClassifyEvent_MachinePreempted(t *testing.T) {
	// Create a Running agent whose pod is gone and machine is Preempted
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Machine:      "test-machine",
			Runtime:      "claude-code",
			Model:        "sonnet",
			DesiredPhase: kyberv1.AgentPhaseRunning,
			Resources:    kyberv1.AgentResources{Disk: resource.MustParse("20Gi")},
		},
		Status: kyberv1.AgentStatus{
			Phase: kyberv1.AgentPhaseRunning,
		},
	}

	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Namespace: "kyber-system"},
		Status: kyberv1.MachineStatus{
			Phase: kyberv1.MachinePhasePreempted,
		},
	}

	r := &AgentReconciler{
		MachineGetter: &fakeMachineGetter{machine: machine},
	}

	event, err := r.classifyEvent(context.Background(), agent, nil)
	if err != nil {
		t.Fatalf("classifyEvent: %v", err)
	}
	if event != EventMachinePreempted {
		t.Errorf("event: got %q, want %q", event, EventMachinePreempted)
	}
}

type fakeMachineGetter struct {
	machine *kyberv1.Machine
}

func (f *fakeMachineGetter) Get(ctx context.Context, name, namespace string) (*kyberv1.Machine, error) {
	return f.machine, nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/agent/ -run TestClassifyEvent_MachinePreempted -v`
Expected: FAIL — MachineGetter field doesn't exist, classification doesn't check machine phase

- [ ] **Step 3: Add MachineGetter interface and preemption check to classifyEvent**

In `pkg/controllers/agent/reconciler.go`:

Add interface:
```go
type MachineGetter interface {
	Get(ctx context.Context, name, namespace string) (*kyberv1.Machine, error)
}
```

Add field to `AgentReconciler`:
```go
MachineGetter MachineGetter
```

In `classifyEvent`, where `EventPodDied` would be returned for a Running agent with no pod (around line 316-318), add a machine check BEFORE returning EventPodDied:

```go
case kyberv1.AgentPhaseRunning:
	if pod == nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		// Check if pod death is caused by machine preemption
		if r.isMachinePreempted(ctx, agent) {
			return EventMachinePreempted, nil
		}
		return EventPodDied, nil
	}
```

Add the same check for Starting phase (around line 299-301).

Add the helper:
```go
func (r *AgentReconciler) isMachinePreempted(ctx context.Context, agent *kyberv1.Agent) bool {
	if r.MachineGetter == nil || agent.Spec.Machine == "" {
		return false
	}
	machine, err := r.MachineGetter.Get(ctx, agent.Spec.Machine, agent.Namespace)
	if err != nil {
		return false
	}
	switch machine.Status.Phase {
	case kyberv1.MachinePhasePreempted, kyberv1.MachinePhaseReplacing, kyberv1.MachinePhaseProvisioning:
		return true
	}
	return false
}
```

- [ ] **Step 4: Implement action handlers for ActionDrainAgent and ActionTransitionToWaiting**

In the action handler switch (reconciler.go), add:

```go
case ActionDrainAgent:
	// Write brief with preemption context, then delete pod with short grace period
	brief := r.buildPreemptionBrief(ctx, agent)
	if err := r.BriefStore.Put(ctx, agent.Name, brief); err != nil {
		log.Error(err, "failed to write preemption brief")
	}
	if agent.Status.PodName != "" {
		grace := int64(20)
		err := r.Client.Delete(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      agent.Status.PodName,
				Namespace: agent.Namespace,
			},
		}, &client.DeleteOptions{GracePeriodSeconds: &grace})
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil

case ActionTransitionToWaiting:
	// No retry counter increment — this is infra, not a bug
	agent.Status.Message = "Waiting for machine replacement after preemption"
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
```

Add the brief builder:
```go
func (r *AgentReconciler) buildPreemptionBrief(ctx context.Context, agent *kyberv1.Agent) briefstore.Brief {
	machine, _ := r.MachineGetter.Get(ctx, agent.Spec.Machine, agent.Namespace)
	var pctx *briefstore.PreemptionContext
	if machine != nil {
		pctx = &briefstore.PreemptionContext{
			InstanceId:    machine.Status.InstanceId,
			Zone:          machine.Spec.Zone,
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
			GracefulDrain: true,
		}
	}
	return briefstore.Brief{
		Version:       1,
		AgentName:     agent.Name,
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		ShutdownType:  briefstore.ShutdownTypePreemption,
		RestartReason: briefstore.RestartReasonPreemption,
		Metadata: briefstore.BriefMetadata{
			PreviousModel:     agent.Status.CurrentModel,
			UptimeSeconds:     r.uptimeSeconds(agent),
			RestartCount:      agent.Status.RestartCount,
			PreemptionContext: pctx,
		},
	}
}
```

- [ ] **Step 5: Add WaitingForMachine requeue logic**

In the main `Reconcile` function, add handling for WaitingForMachine phase. When the reconciler runs for an agent in WaitingForMachine, check if the machine is Ready:

```go
case kyberv1.AgentPhaseWaitingForMachine:
	if r.isMachineReady(ctx, agent) {
		event = EventMachineReady
	} else {
		// Requeue — keep checking
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
```

Add helper:
```go
func (r *AgentReconciler) isMachineReady(ctx context.Context, agent *kyberv1.Agent) bool {
	if r.MachineGetter == nil || agent.Spec.Machine == "" {
		return false
	}
	machine, err := r.MachineGetter.Get(ctx, agent.Spec.Machine, agent.Namespace)
	if err != nil {
		return false
	}
	return machine.Status.Phase == kyberv1.MachinePhaseReady || machine.Status.Phase == kyberv1.MachinePhaseRunning
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/agent/ -run TestClassifyEvent_MachinePreempted -v`
Expected: PASS

- [ ] **Step 7: Run full agent controller tests**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/agent/ -v`
Expected: All tests PASS (may need to update existing tests that construct AgentReconciler to include MachineGetter)

- [ ] **Step 8: Commit**

```bash
git add pkg/controllers/agent/reconciler.go pkg/controllers/agent/reconciler_test.go
git commit -m "feat(agent): preemption-aware event classification and drain/waiting actions"
```

---

### Task 5: Internal API — Preemption Notice Endpoint

**Files:**
- Modify: `pkg/api/internal.go` (add route and handler)
- Test: `pkg/api/internal_test.go`

- [ ] **Step 1: Write failing test for preemption notice endpoint**

```go
// pkg/api/internal_test.go

func TestPreemptionNotice(t *testing.T) {
	store := briefstore.NewMemoryStore()
	notified := make(chan string, 1)
	srv := NewInternalServer(store, WithPreemptionHandler(func(machineName, instanceId string) {
		notified <- machineName
	}))

	body := `{"timestamp":"2026-04-13T12:00:00Z","instanceId":"kyber-testvm"}`
	req := httptest.NewRequest("POST", "/internal/machines/test-machine/preemption-notice", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}

	select {
	case name := <-notified:
		if name != "test-machine" {
			t.Errorf("machine name: got %q, want %q", name, "test-machine")
		}
	case <-time.After(time.Second):
		t.Fatal("preemption handler not called")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/kyber && go test ./pkg/api/ -run TestPreemptionNotice -v`
Expected: FAIL — `WithPreemptionHandler` and route don't exist

- [ ] **Step 3: Add preemption notice route and handler**

In `pkg/api/internal.go`:

Add `PreemptionHandler` callback type and option:
```go
type PreemptionHandler func(machineName, instanceId string)

type InternalServerOption func(*InternalServer)

func WithPreemptionHandler(h PreemptionHandler) InternalServerOption {
	return func(s *InternalServer) {
		s.preemptionHandler = h
	}
}
```

Add field to InternalServer:
```go
type InternalServer struct {
	store              briefstore.BriefStore
	server             *http.Server
	preemptionHandler  PreemptionHandler
}
```

Add route in the mux setup:
```go
mux.HandleFunc("/internal/machines/", s.handleMachineRoutes)
```

Add handler:
```go
func (s *InternalServer) handleMachineRoutes(w http.ResponseWriter, r *http.Request) {
	// Parse: /internal/machines/{name}/preemption-notice
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/internal/machines/"), "/")
	if len(parts) != 2 || parts[1] != "preemption-notice" {
		http.NotFound(w, r)
		return
	}
	machineName := parts[0]

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Timestamp  string `json:"timestamp"`
		InstanceId string `json:"instanceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if s.preemptionHandler != nil {
		s.preemptionHandler(machineName, req.InstanceId)
	}

	w.WriteHeader(http.StatusOK)
}
```

Implement `ServeHTTP` on InternalServer if not already present (delegate to internal mux).

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/dev/kyber && go test ./pkg/api/ -run TestPreemptionNotice -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/api/internal.go pkg/api/internal_test.go
git commit -m "feat(api): add POST /internal/machines/{name}/preemption-notice endpoint"
```

---

### Task 6: Machine Controller — Wire Preemption Notice to Agent Drain

**Files:**
- Modify: `pkg/controllers/machine/reconciler.go`
- Test: `pkg/controllers/machine/reconciler_test.go`

- [ ] **Step 1: Write failing test for preemption notice handling**

```go
// pkg/controllers/machine/reconciler_test.go

func TestPreemptionNotice_DrainsAgents(t *testing.T) {
	machine := &kyberv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Namespace: "kyber-system"},
		Status: kyberv1.MachineStatus{
			Phase:      kyberv1.MachinePhaseRunning,
			InstanceId: "kyber-testvm",
		},
	}

	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "kyber-system"},
		Spec:   kyberv1.AgentSpec{Machine: "test-machine"},
		Status: kyberv1.AgentStatus{Phase: kyberv1.AgentPhaseRunning},
	}

	r := newTestMachineReconciler(machine, agent)
	r.HandlePreemptionNotice(context.Background(), "test-machine", "kyber-testvm")

	// Verify agent was updated with preemption annotation
	updatedAgent := &kyberv1.Agent{}
	err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(agent), updatedAgent)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if updatedAgent.Annotations["kyber.dev/preemption-notice"] == "" {
		t.Error("expected preemption-notice annotation on agent")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/machine/ -run TestPreemptionNotice_DrainsAgents -v`
Expected: FAIL — `HandlePreemptionNotice` doesn't exist

- [ ] **Step 3: Implement HandlePreemptionNotice**

In `pkg/controllers/machine/reconciler.go`:

```go
// HandlePreemptionNotice is called by the internal API when a node agent
// detects an imminent preemption. It annotates all agents on the machine
// so the agent reconciler can fire EventPreemptionNotice on next reconcile.
func (r *MachineReconciler) HandlePreemptionNotice(ctx context.Context, machineName, instanceId string) {
	log := log.FromContext(ctx).WithValues("machine", machineName)
	log.Info("preemption notice received", "instanceId", instanceId)

	// List agents assigned to this machine
	var agents kyberv1.AgentList
	if err := r.Client.List(ctx, &agents,
		client.InNamespace(r.Namespace),
		client.MatchingFields{"spec.machine": machineName},
	); err != nil {
		log.Error(err, "failed to list agents for preemption drain")
		return
	}

	// Annotate each agent to trigger drain on next reconcile
	for i := range agents.Items {
		agent := &agents.Items[i]
		if agent.Status.Phase != kyberv1.AgentPhaseRunning &&
			agent.Status.Phase != kyberv1.AgentPhaseStarting {
			continue
		}
		if agent.Annotations == nil {
			agent.Annotations = make(map[string]string)
		}
		agent.Annotations["kyber.dev/preemption-notice"] = time.Now().UTC().Format(time.RFC3339)
		if err := r.Client.Update(ctx, agent); err != nil {
			log.Error(err, "failed to annotate agent for drain", "agent", agent.Name)
		}
	}
}
```

- [ ] **Step 4: Wire HandlePreemptionNotice into the agent reconciler's classifyEvent**

In `pkg/controllers/agent/reconciler.go`, in `classifyEvent` for Running and Starting phases, check for the annotation:

```go
// Check for preemption notice annotation (set by machine controller)
if agent.Annotations["kyber.dev/preemption-notice"] != "" {
	// Clear the annotation
	delete(agent.Annotations, "kyber.dev/preemption-notice")
	if err := r.Client.Update(ctx, agent); err != nil {
		return "", err
	}
	return EventPreemptionNotice, nil
}
```

This check should be BEFORE the pod status checks, so the drain starts even if the pod is still running.

- [ ] **Step 5: Run tests**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/machine/ -run TestPreemptionNotice -v`
Expected: PASS

Run: `cd ~/dev/kyber && go test ./pkg/controllers/... -v`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
git add pkg/controllers/machine/reconciler.go pkg/controllers/machine/reconciler_test.go pkg/controllers/agent/reconciler.go
git commit -m "feat(machine): wire preemption notice to agent drain via annotation"
```

---

### Task 7: Node Agent — Preemption Watcher

**Files:**
- Create: `pkg/nodeagent/preemption.go`
- Create: `pkg/nodeagent/preemption_test.go`
- Modify: `cmd/node-agent/main.go`

- [ ] **Step 1: Write failing test for preemption watcher**

```go
// pkg/nodeagent/preemption_test.go

func TestPreemptionWatcher_DetectsPreemption(t *testing.T) {
	// Fake metadata server that returns TRUE
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/computeMetadata/v1/instance/preempted" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Metadata-Flavor") != "Google" {
			t.Error("missing Metadata-Flavor header")
		}
		fmt.Fprint(w, "TRUE")
	}))
	defer server.Close()

	notified := make(chan struct{}, 1)
	w := &PreemptionWatcher{
		MetadataURL:      server.URL + "/computeMetadata/v1/instance/preempted",
		PollInterval:     50 * time.Millisecond,
		OnPreemption: func() {
			notified <- struct{}{}
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go w.Run(ctx)

	select {
	case <-notified:
		// Success
	case <-ctx.Done():
		t.Fatal("preemption not detected within timeout")
	}
}

func TestPreemptionWatcher_IgnoresNonPreemption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "FALSE")
	}))
	defer server.Close()

	notified := make(chan struct{}, 1)
	w := &PreemptionWatcher{
		MetadataURL:  server.URL + "/computeMetadata/v1/instance/preempted",
		PollInterval: 50 * time.Millisecond,
		OnPreemption: func() {
			notified <- struct{}{}
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	go w.Run(ctx)

	select {
	case <-notified:
		t.Fatal("should not have detected preemption")
	case <-ctx.Done():
		// Success — no preemption detected
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/kyber && go test ./pkg/nodeagent/ -run TestPreemptionWatcher -v`
Expected: FAIL — `PreemptionWatcher` doesn't exist

- [ ] **Step 3: Implement PreemptionWatcher**

```go
// pkg/nodeagent/preemption.go
package nodeagent

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultMetadataURL  = "http://metadata.google.internal/computeMetadata/v1/instance/preempted"
	DefaultPollInterval = 5 * time.Second
)

type PreemptionWatcher struct {
	MetadataURL  string
	PollInterval time.Duration
	OnPreemption func()
}

func (w *PreemptionWatcher) Run(ctx context.Context) {
	url := w.MetadataURL
	if url == "" {
		url = DefaultMetadataURL
	}
	interval := w.PollInterval
	if interval == 0 {
		interval = DefaultPollInterval
	}

	client := &http.Client{Timeout: 3 * time.Second}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.isPreempted(ctx, client, url) {
				if w.OnPreemption != nil {
					w.OnPreemption()
				}
				return // Only fire once
			}
		}
	}
}

func (w *PreemptionWatcher) isPreempted(ctx context.Context, client *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(body)) == "TRUE"
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd ~/dev/kyber && go test ./pkg/nodeagent/ -run TestPreemptionWatcher -v`
Expected: PASS

- [ ] **Step 5: Wire into node agent main**

In `cmd/node-agent/main.go`, add a goroutine alongside the metrics loop in the default (non-action) mode:

```go
// Preemption watcher — calls control plane when GCE signals preemption
machineName := os.Getenv("KYBER_MACHINE_NAME")
controlPlaneURL := os.Getenv("KYBER_CONTROL_PLANE_URL")
if machineName != "" && controlPlaneURL != "" {
	pw := &nodeagent.PreemptionWatcher{
		OnPreemption: func() {
			log.Println("preemption detected, notifying control plane")
			notifyControlPlane(controlPlaneURL, machineName)
		},
	}
	go pw.Run(ctx)
}
```

Add the notification helper:
```go
func notifyControlPlane(baseURL, machineName string) {
	url := fmt.Sprintf("%s/internal/machines/%s/preemption-notice", baseURL, machineName)
	body := fmt.Sprintf(`{"timestamp":"%s","instanceId":"%s"}`,
		time.Now().UTC().Format(time.RFC3339), os.Getenv("HOSTNAME"))
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		log.Printf("failed to notify control plane: %v", err)
		return
	}
	resp.Body.Close()
	log.Printf("control plane notified: %d", resp.StatusCode)
}
```

- [ ] **Step 6: Commit**

```bash
git add pkg/nodeagent/preemption.go pkg/nodeagent/preemption_test.go cmd/node-agent/main.go
git commit -m "feat(node-agent): add preemption watcher polling GCE metadata endpoint"
```

---

### Task 8: Helm Chart — GCE PD CSI Driver and StorageClass

**Files:**
- Create: `deploy/helm/kyber/templates/storage/storageclass.yaml`
- Modify: `deploy/helm/kyber/values.yaml`
- Modify: `deploy/helm/kyber/templates/node-agent/daemonset.yaml`

- [ ] **Step 1: Add StorageClass template**

```yaml
# deploy/helm/kyber/templates/storage/storageclass.yaml
{{- if .Values.storage.gcePD.enabled }}
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: {{ .Values.storage.gcePD.storageClassName }}
provisioner: pd.csi.storage.gke.io
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
parameters:
  type: {{ .Values.storage.gcePD.diskType }}
  replication-type: {{ .Values.storage.gcePD.replicationType }}
{{- end }}
```

- [ ] **Step 2: Add storage values to values.yaml**

In `deploy/helm/kyber/values.yaml`, add a new storage section:

```yaml
storage:
  gcePD:
    enabled: true
    storageClassName: kyber-pd
    diskType: pd-standard
    replicationType: regional-pd
  # CSI driver is installed separately — see docs/installation.md
```

- [ ] **Step 3: Add preemption env vars to node-agent DaemonSet**

In `deploy/helm/kyber/templates/node-agent/daemonset.yaml`, add to the container env:

```yaml
- name: KYBER_MACHINE_NAME
  valueFrom:
    fieldRef:
      fieldPath: spec.nodeName
- name: KYBER_CONTROL_PLANE_URL
  value: "http://{{ include "kyber.fullname" . }}-control-plane.{{ .Release.Namespace }}.svc:8082"
```

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/kyber/templates/storage/ deploy/helm/kyber/values.yaml deploy/helm/kyber/templates/node-agent/daemonset.yaml
git commit -m "feat(helm): add GCE regional PD StorageClass and node-agent preemption env vars"
```

---

### Task 9: Pod Builder — Use GCE PD StorageClass

**Files:**
- Modify: `pkg/controllers/agent/pod_builder.go` (BuildPVC, line 210-237)
- Test: `pkg/controllers/agent/pod_builder_test.go`

- [ ] **Step 1: Write failing test**

```go
// pkg/controllers/agent/pod_builder_test.go

func TestBuildPVC_DefaultStorageClass(t *testing.T) {
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "kyber-system"},
		Spec: kyberv1.AgentSpec{
			Resources: kyberv1.AgentResources{
				Disk: resource.MustParse("20Gi"),
			},
		},
	}

	pvc := BuildPVC(agent, "")
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != DefaultStorageClass {
		got := "<nil>"
		if pvc.Spec.StorageClassName != nil {
			got = *pvc.Spec.StorageClassName
		}
		t.Errorf("StorageClassName: got %q, want %q", got, DefaultStorageClass)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/agent/ -run TestBuildPVC_DefaultStorageClass -v`
Expected: FAIL — `DefaultStorageClass` undefined

- [ ] **Step 3: Add default storage class constant**

In `pkg/controllers/agent/pod_builder.go`:

```go
const DefaultStorageClass = "kyber-pd"
```

Update `BuildPVC` to use the default when no class is specified:

```go
func BuildPVC(agent *kyberv1.Agent, storageClassName string) *corev1.PersistentVolumeClaim {
	if storageClassName == "" {
		storageClassName = DefaultStorageClass
	}
	// ... rest unchanged
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/agent/ -run TestBuildPVC_DefaultStorageClass -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `cd ~/dev/kyber && go test ./pkg/controllers/agent/ -v`
Expected: All PASS (existing tests that pass a storageClassName are unaffected)

- [ ] **Step 6: Commit**

```bash
git add pkg/controllers/agent/pod_builder.go pkg/controllers/agent/pod_builder_test.go
git commit -m "feat(agent): default to kyber-pd StorageClass for agent PVCs"
```

---

### Task 10: Claude Code Runtime — Preemption Resume Notification

**Files:**
- Modify: `images/claude-code/start-claude.sh`

- [ ] **Step 1: Add session brief reading and preemption env var**

In `images/claude-code/start-claude.sh`, after the OAuth section and before "Build arguments", add:

```bash
# ---- Session brief — preemption detection ----
BRIEF_FILE="/persist/session-brief.json"
if [ -f "$BRIEF_FILE" ]; then
    SHUTDOWN_TYPE=$(jq -r '.shutdownType // ""' "$BRIEF_FILE" 2>/dev/null || echo "")
    if [ "$SHUTDOWN_TYPE" = "preemption" ]; then
        export KYBER_PREEMPTION_RESTART=true
        GRACEFUL=$(jq -r '.metadata.preemptionContext.gracefulDrain // false' "$BRIEF_FILE" 2>/dev/null || echo "false")
        echo "[kyber] Restarting after preemption (graceful=$GRACEFUL)"
    else
        echo "[kyber] Session brief: shutdownType=$SHUTDOWN_TYPE"
    fi
fi
```

- [ ] **Step 2: Verify jq is available in the image**

Check `images/agent-base/Dockerfile` — jq is already installed (line 11).

- [ ] **Step 3: Commit**

```bash
git add images/claude-code/start-claude.sh
git commit -m "feat(claude-code): read session brief and set KYBER_PREEMPTION_RESTART on preemption"
```

---

### Task 11: GCE PD CSI Driver Installation Docs

**Files:**
- Modify: `docs/installation.md`

- [ ] **Step 1: Add CSI driver installation instructions**

Add a section to `docs/installation.md`:

```markdown
## GCE Persistent Disk CSI Driver (Required for k3s)

k3s does not bundle the GCE PD CSI driver. Install it before deploying Kyber:

\`\`\`bash
# Install the GCE PD CSI driver
kubectl apply -k "github.com/kubernetes-sigs/gcp-compute-persistent-disk-csi-driver/deploy/kubernetes/overlays/stable?ref=v1.15.0"

# Verify the driver is running
kubectl get pods -n gce-pd-csi-driver
\`\`\`

The driver requires GCE credentials. On GCE VMs, it uses the instance's service account automatically. For non-GCE clusters, configure credentials per the [driver documentation](https://github.com/kubernetes-sigs/gcp-compute-persistent-disk-csi-driver).
```

- [ ] **Step 2: Commit**

```bash
git add docs/installation.md
git commit -m "docs: add GCE PD CSI driver installation instructions"
```

---

### Task 12: Integration Test — Preemption Flow

**Files:**
- Create: `test/integration/preemption_test.go`

- [ ] **Step 1: Write integration test for the full preemption flow**

```go
// test/integration/preemption_test.go

func TestPreemptionFlow(t *testing.T) {
	// Setup: create a Running agent on a Running machine
	machine := createTestMachine(t, "test-machine", kyberv1.MachinePhaseRunning)
	agent := createTestAgent(t, "test-agent", "test-machine", kyberv1.AgentPhaseRunning)

	// Simulate: preemption notice received
	resp, err := http.Post(
		fmt.Sprintf("%s/internal/machines/test-machine/preemption-notice", internalURL),
		"application/json",
		strings.NewReader(`{"timestamp":"2026-04-13T12:00:00Z","instanceId":"test-instance"}`),
	)
	if err != nil {
		t.Fatalf("preemption notice: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preemption notice status: %d", resp.StatusCode)
	}

	// Wait for agent to reach WaitingForMachine
	waitForAgentPhase(t, "test-agent", kyberv1.AgentPhaseWaitingForMachine, 30*time.Second)

	// Verify brief was written with preemption context
	brief, err := briefStore.Get(context.Background(), "test-agent")
	if err != nil {
		t.Fatalf("get brief: %v", err)
	}
	if brief.ShutdownType != briefstore.ShutdownTypePreemption {
		t.Errorf("brief ShutdownType: got %q, want %q", brief.ShutdownType, briefstore.ShutdownTypePreemption)
	}

	// Verify restart count was NOT incremented
	updatedAgent := getAgent(t, "test-agent")
	if updatedAgent.Status.RestartCount != agent.Status.RestartCount {
		t.Errorf("RestartCount: got %d, want %d (should not increment on preemption)",
			updatedAgent.Status.RestartCount, agent.Status.RestartCount)
	}

	// Simulate: machine replaced and ready
	updateMachinePhase(t, "test-machine", kyberv1.MachinePhaseReady)

	// Wait for agent to resume
	waitForAgentPhase(t, "test-agent", kyberv1.AgentPhaseStarting, 30*time.Second)
}
```

- [ ] **Step 2: Run integration test**

Run: `cd ~/dev/kyber && go test ./test/integration/ -run TestPreemptionFlow -v -tags integration`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add test/integration/preemption_test.go
git commit -m "test: integration test for full preemption flow"
```
