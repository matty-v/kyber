package selfupgrade

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func TestPreActivationUpgradeDoesNotCreateUnrecoverableMaintenance(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: 404})
	}))
	defer server.Close()
	coordinator, err := newUpgradeCredentialCoordinator(&rest.Config{Host: server.URL}, "kyber-system", 0)
	if err != nil {
		t.Fatal(err)
	}
	if coordinator.MaintenanceRequired() {
		t.Fatal("protocol zero enabled persistent maintenance")
	}
	cmd := &fakeCommander{failOn: "upgrade kyber-canary"}
	r := newRunner(t, cmd, &fakeVerifier{})
	r.CredentialCoordinator = coordinator
	err = r.Run(context.Background())
	if err == nil || !cmd.ran("rollback") || strings.Contains(err.Error(), "maintenance retained") {
		t.Fatal("pre-activation retry behavior regressed", err)
	}
	if writes.Load() != 0 {
		t.Fatal("protocol zero created a maintenance resource")
	}
}

func TestPreActivationUpgradeFailsClosedOnUnverifiableAuthority(t *testing.T) {
	const private = "private-authority-diagnostic-canary"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Message: private, Code: 403})
	}))
	defer server.Close()
	coordinator, err := newUpgradeCredentialCoordinator(&rest.Config{Host: server.URL}, "kyber-system", 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := &fakeCommander{}
	r := newRunner(t, cmd, &fakeVerifier{})
	r.CredentialCoordinator = coordinator
	if err := r.Run(context.Background()); err == nil || strings.Contains(err.Error(), private) {
		t.Fatal("authority outage ignored or exposed", err)
	}
	if len(cmd.calls) != 0 {
		t.Fatal("unverified authority reached Helm")
	}
}
