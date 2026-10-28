package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func replicaProbeFixture(t *testing.T, change func(*Server), wrap func(http.Handler) http.Handler) (*credentialAdoptionSources, corev1.Pod, *credentialAdoptionReplicaProbe) {
	t.Helper()
	reader, _ := credentialSourcesFixture(t)
	source, err := reader.Read(context.Background())
	if err != nil {
		t.Fatal("source fixture failed")
	}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: source.namespace, Name: "control-plane-test", UID: "33333333-3333-4333-8333-333333333333"}, Status: corev1.PodStatus{PodIP: "10.0.0.2", Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	server := &Server{Namespace: source.namespace, APIKey: source.root, Callers: append([]ScopedCaller(nil), source.callers...), OperatorCredentialPodUID: string(pod.UID)}
	if change != nil {
		change(server)
	}
	handler := server.BuildHandler()
	if wrap != nil {
		handler = wrap(handler)
	}
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	probe := newCredentialAdoptionReplicaProbe()
	transport := probe.http.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "10.0.0.2:8080" {
			return nil, errors.New("unexpected destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, backend.Listener.Addr().String())
	}
	return source, pod, probe
}

func TestCredentialAdoptionReplicaVerifiesEveryMaterialAndInventory(t *testing.T) {
	source, pod, probe := replicaProbeFixture(t, nil, nil)
	if err := probe.verify(context.Background(), source, pod, 8080); err != nil {
		t.Fatal("current source acceptance did not verify")
	}
	for _, change := range []struct {
		name   string
		change func(*Server)
	}{
		{"stale identity", func(s *Server) { s.Callers[0].PrincipalID = "changed" }},
		{"narrowed access", func(s *Server) { s.Callers[1].AgentResources = []string{"kyber-system/other"} }},
		{"swapped values", func(s *Server) { s.Callers[0].Key, s.Callers[1].Key = s.Callers[1].Key, s.Callers[0].Key }},
		{"extra accepted material", func(s *Server) {
			s.Callers = append(s.Callers, ScopedCaller{Name: "Extra client", Key: "extra-test", Scopes: []string{"metrics:read"}})
		}},
		{"replaced replica", func(s *Server) { s.OperatorCredentialPodUID = "44444444-4444-4444-8444-444444444444" }},
		{"missing replica identity", func(s *Server) { s.OperatorCredentialPodUID = "" }},
		{"retired material", func(s *Server) { s.Callers = s.Callers[:1] }},
	} {
		t.Run(change.name, func(t *testing.T) {
			source, pod, probe := replicaProbeFixture(t, change.change, nil)
			if !errors.Is(probe.verify(context.Background(), source, pod, 8080), errCredentialAdoptionReplica) {
				t.Fatal("changed acceptance verified")
			}
		})
	}
}

func TestCredentialAdoptionReplicaRejectsUnsafeResponsesAndTargets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.1/collect", 302) }},
		{"unavailable", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }},
		{"malformed", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte("invalid"))
		}},
		{"oversize", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(make([]byte, 1<<20+1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, pod, probe := replicaProbeFixture(t, nil, func(http.Handler) http.Handler { return tc.handler })
			if !errors.Is(probe.verify(context.Background(), source, pod, 8080), errCredentialAdoptionReplica) {
				t.Fatal("unsafe response accepted")
			}
		})
	}
	source, pod, probe := replicaProbeFixture(t, nil, nil)
	for _, ip := range []string{"", "localhost", "127.0.0.1", "0.0.0.0", "224.0.0.1", "::1"} {
		changed := pod
		changed.Status.PodIP = ip
		if probe.verify(context.Background(), source, changed, 8080) == nil {
			t.Fatal("unsafe IP accepted")
		}
	}
	changed := pod
	changed.Status.Conditions = nil
	if probe.verify(context.Background(), source, changed, 8080) == nil {
		t.Fatal("unready pod accepted")
	}
	changed = pod
	now := metav1.Now()
	changed.DeletionTimestamp = &now
	if probe.verify(context.Background(), source, changed, 8080) == nil {
		t.Fatal("terminating pod accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if probe.verify(cancelled, source, pod, 8080) == nil {
		t.Fatal("cancelled probe accepted")
	}
}

func TestCredentialAdoptionReplicaChecksSourcesAgainAndDoesNotFollowRedirect(t *testing.T) {
	var calls atomic.Int32
	var activeSource atomic.Pointer[credentialAdoptionSources]
	var changed atomic.Bool
	s, pod, probe := replicaProbeFixture(t, nil, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Path == "/api/v1/operator-credentials" && changed.CompareAndSwap(false, true) {
				secret := &corev1.Secret{}
				source := activeSource.Load()
				key := client.ObjectKey{Namespace: source.namespace, Name: "root-source"}
				if source.reader.Get(r.Context(), key, secret) != nil {
					t.Error("source read failed")
				}
				secret.Data["api-key"] = []byte("changed-test")
				if source.reader.Update(r.Context(), secret) != nil {
					t.Error("source update failed")
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	activeSource.Store(s)
	if probe.verify(context.Background(), s, pod, 8080) == nil || calls.Load() != 4 {
		t.Fatal("source change during verification accepted")
	}
}

func TestCredentialAdoptionReplicaDisablesRedirectsAndBoundsCancellation(t *testing.T) {
	var requests atomic.Int32
	source, pod, probe := replicaProbeFixture(t, nil, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); http.Redirect(w, r, "/collect", 302) })
	})
	if probe.http.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("environment proxy could receive credential probes")
	}
	if probe.verify(context.Background(), source, pod, 8080) == nil || requests.Load() != 1 {
		t.Fatal("redirect was followed or accepted")
	}
	source, pod, probe = replicaProbeFixture(t, nil, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if probe.verify(ctx, source, pod, 8080) == nil || time.Since(start) > time.Second {
		t.Fatal("stalled probe ignored cancellation")
	}
}
