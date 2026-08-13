// Package gceemulator implements the small Compute Engine REST subset used by
// Kyber's GCE adapter. It is for local development and must never be exposed as
// a production service.
package gceemulator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/matty-v/kyber/pkg/adapters"
)

type instance struct {
	ID          uint64
	Name        string
	Zone        string
	Status      string
	Preemptible bool
	MachineType string
	CreatedAt   time.Time
	InternalIP  string
	ExternalIP  string
	Spec        adapters.MachineSpec
}

type Emulator struct {
	mu         sync.Mutex
	instances  map[string]*instance
	nextErrors map[string]bool
	nextID     atomic.Uint64
	nextOp     atomic.Uint64
	server     *http.Server
}

func New() *Emulator {
	e := &Emulator{instances: map[string]*instance{}, nextErrors: map[string]bool{}}
	e.nextID.Store(1000)
	return e
}

func (e *Emulator) Start(ctx context.Context, addr string) error {
	e.server = &http.Server{Addr: addr, Handler: e, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.server.Shutdown(shutdownCtx)
	}()
	if err := e.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (e *Emulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 8 && parts[0] == "compute" && parts[1] == "v1" && parts[2] == "projects" && parts[4] == "zones" && parts[6] == "operations" && r.Method == http.MethodGet {
		e.writeJSON(w, http.StatusOK, map[string]any{"name": parts[7], "status": "DONE", "zone": "zones/" + parts[5]})
		return
	}
	if len(parts) >= 9 && parts[0] == "compute" && parts[1] == "v1" && parts[2] == "projects" && parts[4] == "zones" && parts[6] == "operations" && parts[8] == "wait" {
		e.writeJSON(w, http.StatusOK, e.operation(parts[7], parts[5], 0))
		return
	}
	if len(parts) == 6 && parts[0] == "compute" && parts[1] == "v1" && parts[2] == "projects" && parts[4] == "aggregated" && parts[5] == "instances" {
		e.handleAggregatedList(w, r)
		return
	}
	if len(parts) < 7 || parts[0] != "compute" || parts[1] != "v1" || parts[2] != "projects" || parts[4] != "zones" || parts[6] != "instances" {
		http.NotFound(w, r)
		return
	}
	zone := parts[5]
	if len(parts) == 7 && r.Method == http.MethodPost {
		e.handleInsert(w, r, zone)
		return
	}
	if len(parts) < 8 {
		http.NotFound(w, r)
		return
	}
	name := parts[7]
	action := "get"
	if len(parts) == 9 {
		action = parts[8]
	}
	e.handleInstance(w, r, zone, name, action)
}

func (e *Emulator) handleInsert(w http.ResponseWriter, r *http.Request, zone string) {
	if e.consumeFailure("create") {
		e.writeError(w, "simulated insert failure")
		return
	}
	var body struct {
		Name        string `json:"name"`
		MachineType string `json:"machineType"`
		Scheduling  struct {
			Preemptible       bool   `json:"preemptible"`
			ProvisioningModel string `json:"provisioningModel"`
		} `json:"scheduling"`
		Disks []struct {
			InitializeParams struct {
				DiskSizeGB json.Number `json:"diskSizeGb"`
			} `json:"initializeParams"`
		} `json:"disks"`
		Labels map[string]string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		e.writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid instance"}})
		return
	}
	e.mu.Lock()
	if _, exists := e.instances[body.Name]; exists {
		e.mu.Unlock()
		e.writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{"code": 409, "message": "already exists"}})
		return
	}
	id := e.nextID.Add(1)
	diskSize := 0
	if len(body.Disks) > 0 {
		diskSize, _ = strconv.Atoi(body.Disks[0].InitializeParams.DiskSizeGB.String())
	}
	profile := body.MachineType
	if slash := strings.LastIndex(profile, "/"); slash >= 0 {
		profile = profile[slash+1:]
	}
	interruptible := body.Scheduling.Preemptible || body.Scheduling.ProvisioningModel == "SPOT"
	e.instances[body.Name] = &instance{ID: id, Name: body.Name, Zone: zone, Status: "RUNNING", Preemptible: interruptible, MachineType: body.MachineType, CreatedAt: time.Now().UTC(), InternalIP: "192.0.2.20", ExternalIP: "198.51.100.20", Spec: adapters.MachineSpec{Name: strings.TrimPrefix(body.Name, "kyber-"), Profile: profile, DiskSizeGb: diskSize, Interruptible: interruptible, Location: zone, Labels: body.Labels}}
	e.mu.Unlock()
	e.writeJSON(w, http.StatusOK, e.operation("insert", zone, id))
}

func (e *Emulator) handleInstance(w http.ResponseWriter, r *http.Request, zone, name, action string) {
	op := map[string]string{"start": "start", "stop": "stop", "get": "observe", "delete": "delete"}[action]
	if r.Method == http.MethodDelete {
		op = "delete"
	}
	if e.consumeFailure(op) {
		e.writeError(w, "simulated "+op+" failure")
		return
	}
	e.mu.Lock()
	inst := e.instances[name]
	if inst == nil || inst.Zone != zone {
		e.mu.Unlock()
		e.writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})
		return
	}
	switch {
	case r.Method == http.MethodGet && action == "get":
		payload := e.instanceJSON(inst)
		e.mu.Unlock()
		e.writeJSON(w, http.StatusOK, payload)
	case r.Method == http.MethodPost && action == "start":
		inst.Status = "RUNNING"
		id := inst.ID
		e.mu.Unlock()
		e.writeJSON(w, http.StatusOK, e.operation("start", zone, id))
	case r.Method == http.MethodPost && action == "stop":
		inst.Status = "TERMINATED"
		id := inst.ID
		e.mu.Unlock()
		e.writeJSON(w, http.StatusOK, e.operation("stop", zone, id))
	case r.Method == http.MethodDelete:
		id := inst.ID
		delete(e.instances, name)
		e.mu.Unlock()
		e.writeJSON(w, http.StatusOK, e.operation("delete", zone, id))
	default:
		e.mu.Unlock()
		http.NotFound(w, r)
	}
}

func (e *Emulator) handleAggregatedList(w http.ResponseWriter, _ *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	items := map[string]any{}
	for _, inst := range e.instances {
		key := "zones/" + inst.Zone
		entry, _ := items[key].(map[string]any)
		if entry == nil {
			entry = map[string]any{"instances": []any{}}
			items[key] = entry
		}
		entry["instances"] = append(entry["instances"].([]any), e.instanceJSON(inst))
	}
	e.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (e *Emulator) instanceJSON(inst *instance) map[string]any {
	return map[string]any{"id": strconv.FormatUint(inst.ID, 10), "name": inst.Name, "zone": "zones/" + inst.Zone, "status": inst.Status, "machineType": inst.MachineType, "creationTimestamp": inst.CreatedAt.Format(time.RFC3339), "scheduling": map[string]any{"preemptible": inst.Preemptible, "provisioningModel": map[bool]string{true: "SPOT", false: "STANDARD"}[inst.Preemptible]}, "networkInterfaces": []any{map[string]any{"networkIP": inst.InternalIP, "accessConfigs": []any{map[string]any{"natIP": inst.ExternalIP}}}}}
}

func (e *Emulator) operation(kind, zone string, targetID uint64) map[string]any {
	return map[string]any{"id": strconv.FormatUint(e.nextOp.Add(1), 10), "name": fmt.Sprintf("operation-%s-%d", kind, e.nextOp.Load()), "status": "DONE", "operationType": kind, "targetId": strconv.FormatUint(targetID, 10), "zone": "zones/" + zone}
}

func (e *Emulator) consumeFailure(operation string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	failed := e.nextErrors[operation]
	delete(e.nextErrors, operation)
	return failed
}
func (e *Emulator) writeError(w http.ResponseWriter, message string) {
	e.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"code": 503, "message": message}})
}
func (e *Emulator) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (e *Emulator) ListSimulatedInstances() []adapters.SimulatedInstance {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]adapters.SimulatedInstance, 0, len(e.instances))
	for _, inst := range e.instances {
		out = append(out, adapters.SimulatedInstance{MachineName: strings.TrimPrefix(inst.Name, "kyber-"), ProviderID: strconv.FormatUint(inst.ID, 10), Spec: inst.Spec, Observation: adapters.InstanceObservation{State: state(inst.Status), Interruption: interruption(inst), Location: inst.Zone, InternalIP: inst.InternalIP, ExternalIP: inst.ExternalIP, CreatedAt: inst.CreatedAt}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MachineName < out[j].MachineName })
	return out
}

func (e *Emulator) ApplySimulationScenario(machineName string, scenario adapters.SimulationScenario) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if op := map[adapters.SimulationScenario]string{adapters.SimulationFailNextCreate: "create", adapters.SimulationFailNextStart: "start", adapters.SimulationFailNextStop: "stop", adapters.SimulationFailNextDelete: "delete", adapters.SimulationFailNextObserve: "observe"}[scenario]; op != "" {
		e.nextErrors[op] = true
		return nil
	}
	inst := e.instances["kyber-"+machineName]
	if inst == nil {
		return fmt.Errorf("emulated GCE machine %q not found", machineName)
	}
	switch scenario {
	case adapters.SimulationPending:
		inst.Status = "PROVISIONING"
	case adapters.SimulationRunning:
		inst.Status = "RUNNING"
	case adapters.SimulationStopped, adapters.SimulationPreempted:
		inst.Status = "TERMINATED"
	case adapters.SimulationFailed:
		inst.Status = "UNKNOWN"
	default:
		return fmt.Errorf("unknown simulation scenario %q", scenario)
	}
	if scenario == adapters.SimulationPreempted {
		inst.Preemptible = true
	}
	return nil
}

func state(status string) adapters.InstanceState {
	switch status {
	case "RUNNING":
		return adapters.InstanceStateRunning
	case "TERMINATED":
		return adapters.InstanceStateStopped
	case "PROVISIONING":
		return adapters.InstanceStatePending
	default:
		return adapters.InstanceStateUnknown
	}
}
func interruption(inst *instance) adapters.InterruptionState {
	if inst.Status == "TERMINATED" && inst.Preemptible {
		return adapters.InterruptionPreempted
	}
	return adapters.InterruptionNone
}

var _ adapters.SimulationController = (*Emulator)(nil)
