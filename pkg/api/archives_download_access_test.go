package api_test

import (
	"context"
	"encoding/json"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArchiveDownloadLinkStopsAfterRootRotation(t *testing.T) {
	h := newExportHarness(t)
	id, _ := h.completedExport()
	login := h.do(http.MethodPost, "/api/v1/browser-session", testAPIKey)
	cookies := login.Result().Cookies()
	if login.Code != http.StatusNoContent || len(cookies) != 1 {
		t.Fatal("browser sign-in failed")
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+exportAgent+"/exports/"+id+"/download-link", nil)
	request.AddCookie(cookies[0])
	request.Header.Set("Origin", "http://"+request.Host)
	rr := httptest.NewRecorder()
	h.server.BuildHandler().ServeHTTP(rr, request)
	var link struct{ URL string }
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &link) != nil || link.URL == "" {
		t.Fatal("could not issue download link", rr.Code)
	}
	if rr := h.do(http.MethodHead, link.URL, ""); rr.Code != http.StatusOK {
		t.Fatal("fresh link rejected", rr.Code)
	}
	h.server.APIKeySecretName = "archive-rotation-fixture"
	if err := h.c.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: h.server.APIKeySecretName, Namespace: "kyber-system"}, Data: map[string][]byte{"api-key": []byte(testAPIKey)}}); err != nil {
		t.Fatal(err)
	}
	rotation := h.do(http.MethodPost, "/api/v1/rotate-api-key", testAPIKey)
	var rotated struct {
		APIKey string `json:"apiKey"`
	}
	if rotation.Code != http.StatusOK || json.Unmarshal(rotation.Body.Bytes(), &rotated) != nil || rotated.APIKey == "" {
		t.Fatal("fixture rotation failed")
	}
	rr = h.do(http.MethodGet, link.URL, "")
	if rr.Code != http.StatusForbidden {
		t.Fatal("old link survived root rotation", rr.Code)
	}
	if rr.Header().Get("Cache-Control") != "no-store" || rr.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("download error is cacheable or leaks referrer")
	}
	if rr := h.do(http.MethodGet, "/api/v1/agents/"+exportAgent+"/exports/"+id, rotated.APIKey); rr.Code != http.StatusOK {
		t.Fatal("rotation damaged retained archive", rr.Code)
	}
}
