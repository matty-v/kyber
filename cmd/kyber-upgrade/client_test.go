package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/matty-v/kyber/pkg/selfupgrade"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestUpgradeClientDoesNotNeedDiscoveryForDeploymentGuard(t *testing.T) {
	var discovery atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/agents.kyber.io" && r.Method == http.MethodPatch {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"agents.kyber.io","resourceVersion":"2"}}`))
			return
		}
		if r.URL.Path != "/apis/apps/v1/namespaces/kyber-system/deployments/kyber-control-plane" {
			discovery.Add(1)
			http.Error(w, "discovery unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: "kyber-control-plane", Namespace: "kyber-system"}})
	}))
	defer server.Close()
	config := &rest.Config{Host: server.URL}
	c, err := newUpgradeClient(config)
	if err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "kyber-system", Name: "kyber-control-plane"}, &deployment); err != nil {
		t.Fatal(err)
	}
	crd := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": "agents.kyber.io"}}}
	if _, err := selfupgrade.ApplyCRDs(context.Background(), c, []*unstructured.Unstructured{crd}); err != nil {
		t.Fatal(err)
	}
	if discovery.Load() != 0 {
		t.Fatal("guard depended on discovery")
	}
	if config.Timeout != 0 {
		t.Fatal("shared REST configuration changed")
	}
}
