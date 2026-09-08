package runtimedetect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubReleasesFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
          {"name":"Hermes Agent v0.21.0 (v2026.8.31)","published_at":"2026-08-31T12:00:00Z"},
          {"name":"Hermes Agent v0.21.1 (v2026.9.7)","published_at":"2026-09-07T12:00:00Z"},
          {"name":"Hermes Agent v0.22.0-beta (v2026.9.8)","prerelease":true,"published_at":"2026-09-08T12:00:00Z"},
          {"name":"unparseable","published_at":"2026-09-09T12:00:00Z"}
        ]`))
	}))
	defer server.Close()
	versions, err := NewGitHubReleasesClient(server.URL, 0).Fetch(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0] != "0.21.1" || versions[1] != "0.21.0" {
		t.Fatalf("versions = %v", versions)
	}
}
