package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAuditWriterNeverSniffsUnclassifiedPayloadAsHTML(t *testing.T) {
	for _, early := range []string{"body", "header", "flush"} {
		server := &Server{}
		handler := server.auditSensitiveRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if early == "header" {
				w.WriteHeader(200)
			}
			if early == "flush" {
				w.(http.Flusher).Flush()
			}
			_, _ = io.WriteString(w, r.URL.Query().Get("content"))
		}))
		testServer := httptest.NewServer(handler)
		response, err := testServer.Client().Get(testServer.URL + "?content=" + url.QueryEscape("<script>private-fixture</script>"))
		if err != nil {
			testServer.Close()
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		testServer.Close()
		if err != nil || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain") || response.Header.Get("X-Content-Type-Options") != "nosniff" || string(body) != "<script>private-fixture</script>" {
			t.Fatal("API payload could be interpreted as markup", early, response.Header.Get("Content-Type"))
		}
	}
}
func TestAuditWriterPreservesExplicitContentTypes(t *testing.T) {
	for _, kind := range []string{"application/json", "application/octet-stream", "image/png", "text/event-stream"} {
		server := &Server{}
		handler := server.auditSensitiveRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", kind)
			w.WriteHeader(200)
			_, _ = w.Write([]byte("fixture"))
		}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Header().Get("Content-Type") != kind {
			t.Fatal("declared API representation changed")
		}
	}
}
