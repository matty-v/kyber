package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/matty-v/kyber/internal/testutil"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLiveExecClosesCurrentAccessAndBackend(t *testing.T) {
	for _, mode := range []string{"revoked", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			started, cancelled := make(chan struct{}), make(chan struct{})
			var startOnce, cancelOnce sync.Once
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				startOnce.Do(func() { close(started) })
				select {
				case <-r.Context().Done():
					cancelOnce.Do(func() { close(cancelled) })
				case <-time.After(12 * time.Second):
				}
			}))
			defer backend.Close()
			config := &rest.Config{Host: backend.URL}
			clientset, err := kubernetes.NewForConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			scheme := runtime.NewScheme()
			if err := kyberv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			agent := &kyberv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "kyber-system"}}
			authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
			authority.edit(func(d *operatorcredentials.Document) {
				d.Credentials[2].Scopes = append(d.Credentials[2].Scopes, "agents:exec")
			})
			s := &Server{Namespace: "kyber-system", SecurityAudit: testutil.NewSecurityAudit(), auth: registryFixtureAuthenticator(t, authority), RestConfig: config, Clientset: clientset, K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()}
			server := httptest.NewServer(s.BuildHandler())
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/agents/alpha/exec", http.Header{"Authorization": []string{"Bearer client-key-fixture"}})
			if err != nil {
				t.Fatal("exec websocket did not establish")
			}
			defer conn.Close()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("exec backend was not reached")
			}
			if mode == "revoked" {
				authority.edit(func(d *operatorcredentials.Document) {
					d.Credentials[2] = d.Credentials[2].WithSecretMaterial(nil)
					d.Credentials[2].State = "revoked"
					d.Credentials[2].PermissionRevision++
				})
			} else {
				authority.mu.Lock()
				authority.err = errOperatorAuthorityUnavailable
				authority.mu.Unlock()
			}
			_ = conn.SetReadDeadline(time.Now().Add(9 * time.Second))
			_, _, err = conn.ReadMessage()
			want := websocket.ClosePolicyViolation
			if mode == "unavailable" {
				want = websocket.CloseTryAgainLater
			}
			if !websocket.IsCloseError(err, want) {
				t.Fatalf("wrong access-change close code: %v", err)
			}
			select {
			case <-cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("exec backend retained a revoked connection")
			}
		})
	}
}
