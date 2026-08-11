package api

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/configexport"
)

const exportNS = "kyber-system"

func exportHelmSecret(t *testing.T, config map[string]any) *corev1.Secret {
	t.Helper()
	rel := map[string]any{
		"name":    "kyber-canary",
		"version": 1,
		"config":  config,
		"chart":   map[string]any{"metadata": map[string]any{"version": "1.0.1"}},
	}
	raw, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.kyber-canary.v1",
			Namespace: exportNS,
			Labels:    map[string]string{"owner": "helm"},
		},
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))},
	}
}

func exportServer(objs ...client.Object) *Server {
	c := fake.NewClientBuilder().WithObjects(objs...).Build()
	return &Server{ConfigExporter: &configexport.Reader{Client: c, Namespace: exportNS}}
}

func TestConfigExport_ReturnsRedactedValues(t *testing.T) {
	s := exportServer(exportHelmSecret(t, map[string]any{
		"api": map[string]any{
			"apiKey":         "sk-must-not-leak",
			"existingSecret": "kyber-api-credentials",
		},
	}))
	rr := httptest.NewRecorder()
	s.handleConfigExport(rr, httptest.NewRequest(http.MethodGet, "/api/v1/config/export", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "sk-must-not-leak") {
		t.Error("the API key leaked through the endpoint")
	}
	if !strings.Contains(body, "kyber-api-credentials") {
		t.Error("the Secret name was dropped; the export cannot recreate the cluster")
	}

	var got configexport.Export
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Available {
		t.Errorf("available = false; reason=%q", got.Reason)
	}
	if len(got.RedactedPaths) == 0 {
		t.Error("redactedPaths is empty; the operator needs to know what to supply on restore")
	}
}

// An ArgoCD-managed cluster — every one of Matt's today — has no Helm release.
// That is a 200 with an explanation, not an error.
func TestConfigExport_NonHelmInstallIs200WithAnExplanation(t *testing.T) {
	s := exportServer()
	rr := httptest.NewRecorder()
	s.handleConfigExport(rr, httptest.NewRequest(http.MethodGet, "/api/v1/config/export", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (not being a Helm release is not an error)", rr.Code)
	}
	var got configexport.Export
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Available {
		t.Error("available = true with no Helm release")
	}
	if got.Reason == "" {
		t.Error("reason is empty; the operator needs to be told where the config actually lives")
	}
}

func TestConfigExport_MethodNotAllowed(t *testing.T) {
	s := exportServer()
	rr := httptest.NewRecorder()
	s.handleConfigExport(rr, httptest.NewRequest(http.MethodPost, "/api/v1/config/export", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rr.Code)
	}
}

func TestConfigExport_DisabledReturns503(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleConfigExport(rr, httptest.NewRequest(http.MethodGet, "/api/v1/config/export", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 with no exporter configured", rr.Code)
	}
}
