package runtimedetect_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/runtimedetect"
)

// npmFixture renders a minimal npm registry JSON body with the given
// versions + dist-tags latest. Each version's `time` entry advances by 1
// hour starting at base.
func npmFixture(base time.Time, latest string, versions ...string) string {
	timesJSON := ""
	versionsJSON := ""
	for i, v := range versions {
		if i > 0 {
			timesJSON += ","
			versionsJSON += ","
		}
		t := base.Add(time.Duration(i) * time.Hour).UTC().Format(time.RFC3339)
		timesJSON += fmt.Sprintf("%q:%q", v, t)
		versionsJSON += fmt.Sprintf("%q:{}", v)
	}
	body := `{
  "dist-tags": {"latest": "` + latest + `"},
  "versions": {` + versionsJSON + `},
  "time": {
    "created": "2024-01-01T00:00:00.000Z",
    "modified": "2024-01-02T00:00:00.000Z",
    ` + timesJSON + `
  }
}`
	return body
}

func TestNpmClient_Fetch_RecentVersionsWithLatestFirst(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	body := npmFixture(base, "2.0.0", "1.0.0", "1.5.0", "2.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := runtimedetect.NewNpmClient(srv.URL, 5*time.Second)
	got, err := c.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 versions, got %d: %v", len(got), got)
	}
	if got[0] != "2.0.0" {
		t.Fatalf("expected dist-tags.latest at position 0, got %q (full: %v)", got[0], got)
	}
}

func TestNpmClient_Fetch_LimitTruncates(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	body := npmFixture(base, "0.5.0", "0.1.0", "0.2.0", "0.3.0", "0.4.0", "0.5.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := runtimedetect.NewNpmClient(srv.URL, 5*time.Second)
	got, err := c.Fetch(context.Background(), 3)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 versions (limit), got %d: %v", len(got), got)
	}
	if got[0] != "0.5.0" {
		t.Fatalf("expected latest first, got %q", got[0])
	}
}

func TestNpmClient_Fetch_FiltersPrereleases(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	body := npmFixture(base, "1.0.0", "1.0.0", "1.1.0-beta", "1.2.0-rc.1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := runtimedetect.NewNpmClient(srv.URL, 5*time.Second)
	got, err := c.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, v := range got {
		if v != "1.0.0" {
			t.Fatalf("unexpected non-stable version surfaced: %q (full: %v)", v, got)
		}
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly the 1 stable version, got %v", got)
	}
}

func TestNpmClient_Fetch_Non2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := runtimedetect.NewNpmClient(srv.URL, 5*time.Second)
	if _, err := c.Fetch(context.Background(), 10); err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestNpmClient_Fetch_RespectsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := runtimedetect.NewNpmClient(srv.URL, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Fetch(ctx, 10); err == nil {
		t.Fatal("expected error on canceled context, got nil")
	}
}
