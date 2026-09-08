package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type rewriteTransport struct {
	base http.RoundTripper
	url  string
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = strings.SplitN(r.url, "://", 2)[0]
	clone.URL.Host = strings.SplitN(r.url, "://", 2)[1]
	return r.base.RoundTrip(clone)
}

func TestPost(t *testing.T) {
	var path, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := &http.Client{Transport: rewriteTransport{base: http.DefaultTransport, url: server.URL}}
	if err := post(context.Background(), client, "token-usage", []byte(`{"model":"test"}`)); err != nil {
		t.Fatal(err)
	}
	if path != "/token-usage" || body != `{"model":"test"}` {
		t.Fatalf("path/body = %q/%q", path, body)
	}
}
