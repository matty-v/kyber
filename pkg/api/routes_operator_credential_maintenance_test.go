package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

type maintenanceReadFixture struct {
	namespace string
	status    operatorcredentials.MaintenanceStatus
	err       error
	reads     int
	afterRead func()
}

func (f *maintenanceReadFixture) Namespace() string { return f.namespace }
func (f *maintenanceReadFixture) Inspect(context.Context) (operatorcredentials.MaintenanceStatus, error) {
	f.reads++
	if f.afterRead != nil {
		f.afterRead()
	}
	return f.status, f.err
}
func TestManagedCredentialMaintenanceChecksCurrentAdminAndNamespace(t *testing.T) {
	reader := &maintenanceReadFixture{namespace: "kyber-system", status: operatorcredentials.MaintenanceStatus{Present: true, Kind: "upgrade", ExecutionID: "e703854e-4d01-45d7-a321-5c737d173d46", StartedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), UID: "private-object-id", Version: "opaque-resource-version"}}
	server := &Server{Namespace: "kyber-system", APIKey: "root-fixture", Callers: []ScopedCaller{usageCallerFixture("restricted-fixture", "Reporter")}, OperatorCredentialMaintenance: reader}
	handler := server.BuildHandler()
	request := func(key, method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/v1/operator-credentials/maintenance", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("restricted-fixture", http.MethodGet); w.Code != 403 || reader.reads != 0 {
		t.Fatal("restricted caller inspected maintenance")
	}
	if w := request("root-fixture", http.MethodPost); w.Code != 405 || reader.reads != 0 {
		t.Fatal("inspection accepted mutation method")
	}
	w := request("root-fixture", http.MethodGet)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("ETag") == "" || !strings.Contains(w.Body.String(), `"blocked":true`) || !strings.Contains(w.Body.String(), "2000-01-01") || strings.Contains(w.Body.String(), "private-object-id") || strings.Contains(w.Body.String(), "opaque-resource-version") {
		t.Fatal("invalid inspection projection", w.Code)
	}
	reader.status = operatorcredentials.MaintenanceStatus{}
	if w := request("root-fixture", http.MethodGet); w.Code != 200 || strings.Contains(w.Body.String(), `"operation"`) || !strings.Contains(w.Body.String(), `"blocked":false`) {
		t.Fatal("missing operation was invented")
	}
	reader.err = errors.New("private transport diagnostics")
	if w := request("root-fixture", http.MethodGet); w.Code != 503 || strings.Contains(w.Body.String(), "private transport") || strings.Contains(w.Body.String(), `"blocked":false`) {
		t.Fatal("read outage leaked diagnostics or looked idle")
	}
	reader.err = nil
	reader.afterRead = func() { server.auth.SetKey("replacement-fixture") }
	if w := request("root-fixture", http.MethodGet); w.Code != 403 || strings.Contains(w.Body.String(), "observedAt") {
		t.Fatal("retired administrator received metadata")
	}
	reader.afterRead = nil
	reader.namespace = "other-namespace"
	before := reader.reads
	if w := request("replacement-fixture", http.MethodGet); w.Code != 503 || reader.reads != before {
		t.Fatal("cross namespace maintenance accepted")
	}
}
