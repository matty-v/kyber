package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/activity"
)

const observerTokenPath = "/var/run/secrets/kyber-observer/token"

// Runtime payloads cannot supply the epoch, pod identity, boot identity or
// sequence. No inventory is retained/re-timestamped on a heartbeat tick.
type workInventoryForwarder struct {
	mu           sync.Mutex
	client       *http.Client
	cfg          config
	registration activity.Registration
	epoch        string
	sequence     int64
	tokenPath    string
}

func newWorkInventoryForwarder(client *http.Client, cfg config, podUID string) *workInventoryForwarder {
	return &workInventoryForwarder{client: client, cfg: cfg, registration: activity.Registration{PodUID: podUID, BootID: uuid.NewString(), BootedAt: time.Now().UTC()}, tokenPath: observerTokenPath}
}
func (f *workInventoryForwarder) send(r *http.Request, endpoint string, body any, dst any) int {
	token, err := readPodToken(f.tokenPath)
	if err != nil || token == "" {
		return 503
	}
	data, err := json.Marshal(body)
	if err != nil {
		return 500
	}
	url := strings.TrimRight(f.cfg.ControlPlaneURL, "/") + "/internal/activity-observers/" + f.cfg.AgentName + "/" + endpoint
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return 500
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return 503
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && dst != nil {
		if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(dst) != nil {
			return 502
		}
	}
	return resp.StatusCode
}
func (f *workInventoryForwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input struct {
		ObservedAt   time.Time `json:"observedAt"`
		LastWorkedAt time.Time `json:"lastWorkedAt"`
		Foreground   *int32    `json:"foreground"`
		Background   *int32    `json:"background"`
		Complete     *bool     `json:"complete"`
		NeedsInput   *bool     `json:"needsInput"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil || dec.Decode(&struct{}{}) != io.EOF || input.Foreground == nil || input.Background == nil || input.Complete == nil || input.NeedsInput == nil {
		http.Error(w, "incomplete native inventory", 400)
		return
	}
	snapshot := activity.Snapshot{ObservedAt: input.ObservedAt, LastWorkedAt: input.LastWorkedAt, Foreground: *input.Foreground, Background: *input.Background, Complete: *input.Complete, NeedsInput: *input.NeedsInput}
	if !snapshot.Valid(time.Now()) || snapshot.ObservedAt.Before(f.registration.BootedAt) {
		http.Error(w, "invalid native inventory", 400)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.epoch == "" {
		var registered activity.RegistrationResponse
		code := f.send(r, "register", f.registration, &registered)
		if code != 200 || registered.Epoch == "" {
			http.Error(w, "observer registration unavailable", 503)
			return
		}
		f.epoch = registered.Epoch
		f.sequence = registered.Sequence
	}
	f.sequence++
	code := f.send(r, "inventory", activity.Report{Registration: f.registration, Epoch: f.epoch, Sequence: f.sequence, Snapshot: snapshot}, nil)
	if code < 200 || code >= 300 {
		if code == 409 {
			f.epoch = ""
		}
		http.Error(w, "inventory not accepted", code)
		return
	}
	w.WriteHeader(204)
}
