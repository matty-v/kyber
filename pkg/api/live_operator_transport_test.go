package api

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/taskstore"
)

func TestLiveAccessKeepsRenameAndUnrelatedRotationConnected(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	s := &Server{Namespace: "kyber-system", SecurityAudit: testutil.NewSecurityAudit(), auth: registryFixtureAuthenticator(t, authority)}
	stopped := make(chan struct{})
	handler := authMiddleware(s.auth, s.auditSensitiveRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.requireSensitiveResource(w, r, "alpha", ScopeLogsRead) {
			return
		}
		_, _ = io.WriteString(w, "ready\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	})))
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/agents/alpha/logs", nil)
	caller := mustRegistryCaller(t, s.auth, registryBearer("client-key-fixture"))
	request.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: mustRegistryCookie(t, s.auth, *caller)})
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if line, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatal("stream did not begin", err)
	}
	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[2].Name = "Renamed client"
		d.Credentials[1] = d.Credentials[1].WithSecretMaterial([]byte("unrelated-live-rotation"))
		d.Credentials[1].Generation++
	})
	select {
	case <-stopped:
		t.Fatal("rename or unrelated rotation interrupted this credential")
	case <-time.After(operatorAccessCheckInterval + 500*time.Millisecond):
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("client disconnect did not release stream")
	}
}

func TestLiveAccessInterruptsStalledUpload(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[2].Scopes = append(d.Credentials[2].Scopes, "secrets:write")
	})
	s := &Server{Namespace: "kyber-system", SecurityAudit: testutil.NewSecurityAudit(), auth: registryFixtureAuthenticator(t, authority)}
	started, stopped := make(chan struct{}), make(chan struct{})
	handler := authMiddleware(s.auth, s.auditSensitiveRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.requireSensitiveResource(w, r, "alpha", ScopeSecretsWrite) {
			return
		}
		close(started)
		_, _ = io.ReadFull(r.Body, make([]byte, 1024))
		close(stopped)
	})))
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = io.WriteString(conn, "PUT /api/v1/agents/alpha/secrets/TOKEN HTTP/1.1\r\nHost: fixture\r\nAuthorization: Bearer client-key-fixture\r\nContent-Length: 1024\r\n\r\nx")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upload did not reach body read")
	}
	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[2] = d.Credentials[2].WithSecretMaterial(nil)
		d.Credentials[2].State = "revoked"
		d.Credentials[2].PermissionRevision++
	})
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("revoked uploader kept a blocked body read open")
	}
}

func TestLiveAccessClosesBackpressuredHijackedConnection(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	s := &Server{Namespace: "kyber-system", SecurityAudit: testutil.NewSecurityAudit(), auth: registryFixtureAuthenticator(t, authority)}
	started, stopped := make(chan struct{}), make(chan struct{})
	handler := authMiddleware(s.auth, s.auditSensitiveRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.requireSensitiveResource(w, r, "alpha", ScopeLogsRead) {
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if tcp, ok := conn.UnderlyingConn().(*net.TCPConn); ok {
			_ = tcp.SetWriteBuffer(1024)
		}
		close(started)
		// The client deliberately does not read. Only cancellation of the raw
		// hijacked connection can release this writer; no handler timeout is set.
		_ = conn.WriteMessage(websocket.BinaryMessage, make([]byte, 2*1024*1024))
		close(stopped)
	})))
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/agents/alpha/logs", http.Header{"Authorization": []string{"Bearer client-key-fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tcp, ok := conn.UnderlyingConn().(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("websocket did not start")
	}
	select {
	case <-stopped:
		t.Fatal("backpressure fixture completed before revocation")
	case <-time.After(100 * time.Millisecond):
	}
	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[2].AgentResources = []string{"kyber-system/other"}
		d.Credentials[2].PermissionRevision++
	})
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("revoked hijacked write remained blocked")
	}
}

type idleLiveTaskEvents struct{ taskstore.Store }

func (*idleLiveTaskEvents) EventHighWater(context.Context, taskstore.AgentRef, string, taskstore.AuthorizationContext) (int64, error) {
	return 0, nil
}
func (*idleLiveTaskEvents) EventSnapshot(context.Context, taskstore.AgentRef, string, taskstore.AuthorizationContext) (*taskstore.Task, int64, error) {
	return nil, 0, taskstore.ErrNotFound
}
func (*idleLiveTaskEvents) ReadEvents(_ context.Context, p taskstore.EventReadParams) (*taskstore.EventPage, error) {
	if p.Authorization.PrincipalID != "client@example.invalid" || p.Authorization.TenantID != "tenant_client" {
		return nil, taskstore.ErrNotFound
	}
	return &taskstore.EventPage{Events: []taskstore.TaskEvent{}, HighWater: 0, Terminal: false}, nil
}
func TestLiveTaskEventStreamDoesNotWaitForHeartbeat(t *testing.T) {
	authority := &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	s := &Server{Namespace: "kyber-system", TaskStore: &idleLiveTaskEvents{}, auth: registryFixtureAuthenticator(t, authority)}
	server := httptest.NewServer(s.BuildHandler())
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/agents/alpha/tasks/task_11111111111111111111111111111111/events", nil)
	request.Header.Set("Authorization", "Bearer client-key-fixture")
	request.Header.Set("Accept", "text/event-stream")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("task stream failed", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || !strings.Contains(line, "kyber-task-events") {
		t.Fatal("stream did not establish", err)
	}
	authority.edit(func(d *operatorcredentials.Document) {
		d.Credentials[2].Scopes = []string{"logs:read"}
		d.Credentials[2].PermissionRevision++
	})
	ended := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, reader); close(ended) }()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("task access persisted until the twenty-second heartbeat")
	}
}

func TestLiveWriterHasSafeDirectBrowserDefaults(t *testing.T) {
	for _, early := range []string{"body", "header", "flush"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writer := &liveOperatorWriter{ResponseWriter: w, ctx: r.Context()}
			if early == "header" {
				writer.WriteHeader(200)
			}
			if early == "flush" {
				writer.Flush()
			}
			_, _ = io.WriteString(writer, r.URL.Query().Get("input"))
		}))
		response, err := server.Client().Get(server.URL + "?input=%3Cscript%3Efixture%3C/script%3E")
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		server.Close()
		if err != nil || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain") || response.Header.Get("X-Content-Type-Options") != "nosniff" || string(body) != "<script>fixture</script>" {
			t.Fatal("live writer exposed browser-sniffed markup", early)
		}
	}
}
