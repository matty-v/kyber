//go:build integration

package contract

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

type contractMaintenance struct {
	status operatorcredentials.MaintenanceStatus
}

func (*contractMaintenance) Namespace() string { return contractNS }
func (m *contractMaintenance) Inspect(context.Context) (operatorcredentials.MaintenanceStatus, error) {
	return m.status, nil
}
func (*contractMaintenance) WriterState(context.Context, string, string) (string, error) {
	return operatorcredentials.MaintenanceWriterStopped, nil
}
func (*contractMaintenance) PrepareRecovery(context.Context, string, string, string, string) (operatorcredentials.MaintenanceRecoveryHandle, error) {
	return nil, operatorcredentials.ErrUnavailable
}
func (*contractMaintenance) ResumeRecovery(context.Context, string, operatorcredentials.MaintenanceReleaseProof) (operatorcredentials.MaintenanceRecoveryHandle, error) {
	return nil, operatorcredentials.ErrUnavailable
}
func (*contractMaintenance) Verify(context.Context, operatorcredentials.MaintenanceStatus) error {
	return nil
}

type contractMaintenanceJournal struct{ *contractCredentialJournal }

func (*contractMaintenanceJournal) RecordCredentialMaintenanceEffect(context.Context, string, string, securityaudit.CredentialMaintenanceEffect) (securityaudit.CredentialOperation, error) {
	return securityaudit.CredentialOperation{}, securityaudit.ErrUnavailable
}
func (j *contractMaintenanceJournal) PendingCredentialMaintenanceOperations(context.Context, string, string, int) ([]securityaudit.CredentialOperation, error) {
	if j.operation == nil {
		return nil, nil
	}
	return []securityaudit.CredentialOperation{*j.operation}, nil
}
func (*contractMaintenanceJournal) FinishCredentialMaintenanceOperation(context.Context, string, string, string, time.Time, *securityaudit.CredentialMaintenanceEffect) (securityaudit.Event, error) {
	return securityaudit.Event{}, securityaudit.ErrUnavailable
}

func TestContract_MaintenanceRecovery(t *testing.T) {
	m := &contractMaintenance{status: operatorcredentials.MaintenanceStatus{Present: true, Kind: operatorcredentials.MaintenanceUpgrade, ExecutionID: uuid.NewString(), UID: uuid.NewString(), Version: "1", StartedAt: time.Now(), Writer: &operatorcredentials.MaintenanceWriter{PodName: "upgrade-pod", JobName: "upgrade-job"}}}
	j := &contractMaintenanceJournal{&contractCredentialJournal{SecurityAudit: testutil.NewSecurityAudit()}}
	s := &api.Server{APIKey: contractAPIKey, Namespace: contractNS, SecurityAudit: j, OperatorCredentialMaintenance: m, OperatorCredentialMaintenanceReconciler: m}
	handler := s.BuildHandler()
	for _, path := range []string{"/api/v1/operator-credentials/maintenance", "/api/v1/operator-credentials/maintenance/readiness", "/api/v1/operator-credentials/maintenance/operations/pending"} {
		response := validateContract(t, handler, authedReq(t, http.MethodGet, path, nil))
		if response.Code != 200 {
			t.Fatal("inspection failed", path, response.Code)
		}
	}
	response := validateContract(t, handler, authedReq(t, http.MethodGet, "/api/v1/operator-credentials/maintenance/readiness", nil))
	var readiness struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(response.Body.Bytes(), &readiness) != nil || readiness.Version == "" {
		t.Fatal("missing readiness version")
	}
	id := uuid.NewString()
	request := authedReq(t, http.MethodPost, "/api/v1/operator-credentials/maintenance/recover", map[string]string{"operationId": id})
	request.Header.Set("If-Match", readiness.Version)
	response = validateContract(t, handler, request)
	if response.Code != 202 {
		t.Fatal("uncertain effect should remain pending", response.Code)
	}
	for _, path := range []string{"/api/v1/operator-credentials/maintenance/operations?operationId=" + id, "/api/v1/operator-credentials/maintenance/operations/pending"} {
		response = validateContract(t, handler, authedReq(t, http.MethodGet, path, nil))
		if response.Code != 200 {
			t.Fatal("operation read failed", response.Code)
		}
	}
	response = validateContract(t, handler, authedReq(t, http.MethodPost, "/api/v1/operator-credentials/maintenance/operations/resolve", map[string]string{"operationId": id}))
	if response.Code != 202 {
		t.Fatal("unconfirmed resolution should remain pending", response.Code)
	}
}
