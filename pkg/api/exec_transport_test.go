package api

import (
	"bytes"
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func TestExecTransportPreservesTLSAuthenticationAndSPDYStreams(t *testing.T) {
	var authenticated atomic.Bool
	var proxyConsulted atomic.Bool
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticated.Store(r.Header.Get("Authorization") == "Bearer fixture-kubernetes-token")
		if !authenticated.Load() {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if _, err := httpstream.Handshake(r, w, []string{"v4.channel.k8s.io"}); err != nil {
			return
		}
		conn := spdy.NewResponseUpgrader().UpgradeResponse(w, r, func(stream httpstream.Stream, replySent <-chan struct{}) error {
			go func() {
				<-replySent
				switch stream.Headers().Get(corev1.StreamType) {
				case corev1.StreamTypeStdout:
					_, _ = stream.Write([]byte("fixture output\n"))
				case corev1.StreamTypeError:
					_, _ = stream.Write([]byte(`{"status":"Success"}`))
				}
				_ = stream.Close()
			}()
			return nil
		})
		if conn == nil {
			return
		}
		defer conn.Close()
		select {
		case <-conn.CloseChan():
		case <-time.After(5 * time.Second):
		}
	}))
	defer backend.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})
	config := &rest.Config{Host: backend.URL, BearerToken: "fixture-kubernetes-token", TLSClientConfig: rest.TLSClientConfig{CAData: ca}, Proxy: func(*http.Request) (*url.URL, error) { proxyConsulted.Store(true); return nil, nil }}
	target, _ := url.Parse(backend.URL + "/exec")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor, err := newExecExecutor(ctx, config, target)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &output}); err != nil {
		t.Fatalf("authenticated TLS SPDY stream failed: %v", err)
	}
	if !authenticated.Load() || !proxyConsulted.Load() || output.String() != "fixture output\n" {
		t.Fatal("TLS/authentication/output not preserved")
	}
}

func TestExecTransportRejectsRedirectAndRedactsUpgradeBody(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var forwarded atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
			defer other.Close()
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", other.URL)
				w.WriteHeader(status)
				_, _ = w.Write([]byte("sensitive-upstream-value"))
			}))
			defer backend.Close()
			config := &rest.Config{Host: backend.URL, BearerToken: "fixture-kubernetes-token"}
			target, _ := url.Parse(backend.URL + "/exec")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			executor, err := newExecExecutor(ctx, config, target)
			if err != nil {
				t.Fatal(err)
			}
			err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{})
			if err == nil || strings.Contains(err.Error(), "sensitive-upstream-value") {
				t.Fatal("upgrade failure missing or contains upstream body")
			}
			if forwarded.Load() != 0 {
				t.Fatal("exec followed a credential-bearing redirect")
			}
		})
	}
}

func TestExecTransportRejectsUnverifiedTLSIdentity(t *testing.T) {
	var contacted atomic.Bool
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted.Store(true) }))
	defer backend.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})
	config := &rest.Config{Host: backend.URL, TLSClientConfig: rest.TLSClientConfig{CAData: ca, ServerName: "incorrect-host.invalid"}}
	target, _ := url.Parse(backend.URL + "/exec")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor, err := newExecExecutor(ctx, config, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{}); err == nil {
		t.Fatal("unverified TLS peer was accepted")
	}
	if contacted.Load() {
		t.Fatal("request reached an unverified TLS peer")
	}
}
