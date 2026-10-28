package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

type usageObserverFixture struct {
	mu            sync.Mutex
	observations  []securityaudit.CredentialUsageObservation
	id, namespace string
}

func (o *usageObserverFixture) CredentialUsageObserverID() string { return o.id }
func (o *usageObserverFixture) CredentialUsageNamespace() string  { return o.namespace }
func (o *usageObserverFixture) ObserveCredentialUsage(event securityaudit.CredentialUsageObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.observations = append(o.observations, event)
}
func newUsageObserverFixture() *usageObserverFixture {
	return &usageObserverFixture{id: uuid.NewString(), namespace: "kyber-system"}
}
func usageCallerFixture(key, name string) ScopedCaller {
	return ScopedCaller{Key: key, Name: name, PrincipalID: "usage-principal", TenantID: "usage-tenant", CredentialID: "usage-credential", CredentialGeneration: 1, Scopes: []string{string(ScopeMetricsRead)}}
}

func TestManagedCredentialUsageSeparatesAuthenticationAndDenial(t *testing.T) {
	observer := newUsageObserverFixture()
	auth := NewAPIKeyAuthenticator("root-fixture", usageCallerFixture("restricted-fixture", "Reporter"))
	handler := authMiddleware(auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }), observer)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents/example/secrets?token=not-recorded", nil)
	r.Header.Set("Authorization", "Bearer restricted-fixture")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || len(observer.observations) != 2 {
		t.Fatal("authenticated denial not observed independently")
	}
	accepted, denied := observer.observations[0], observer.observations[1]
	if accepted.Outcome != "accepted" || accepted.Action != "secrets:list" || accepted.CredentialID != "usage-credential" || accepted.TenantID != "usage-tenant" || denied.Outcome != "denied" || denied.CredentialID != accepted.CredentialID {
		t.Fatal("incorrect credential attribution")
	}
}
func TestManagedCredentialUsageAmbiguousValuesRemainUnattributed(t *testing.T) {
	observer := newUsageObserverFixture()
	first, second := usageCallerFixture("first-fixture", "First"), usageCallerFixture("second-fixture", "Second")
	auth := NewAPIKeyAuthenticator("root-fixture", first, second)
	handler := authMiddleware(auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }), observer)
	r := registryBearer("first-fixture")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || len(observer.observations) != 1 || observer.observations[0].Outcome != "unattributed" || observer.observations[0].CredentialID != "" {
		t.Fatal("ambiguous values were guessed or authentication changed")
	}
	for _, view := range auth.credentialInventory() {
		if view.ID == first.CredentialID && (view.ActivityTrackable || view.ActivityUnavailableReason != "identity-shared") {
			t.Fatal("ambiguous inventory advertised separate usage")
		}
	}
	// Matching copies of one value share one actual credential, not two values.
	auth = NewAPIKeyAuthenticator("root-fixture", first, first)
	caller, err := auth.Authenticate(registryBearer("first-fixture"))
	if err != nil || !auth.credentialUsageIdentityUnique(caller) {
		t.Fatal("shadowed identical material lost its actual identity")
	}
}
func TestManagedCredentialUsagePreservesWebSocketAndContentType(t *testing.T) {
	observer := newUsageObserverFixture()
	auth := NewAPIKeyAuthenticator("root-fixture")
	server := httptest.NewServer(authMiddleware(auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exec") {
			connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer connection.Close()
			_ = connection.WriteMessage(websocket.TextMessage, []byte("ready"))
			return
		}
		_, _ = w.Write([]byte("<script>fixture</script>"))
	}), observer))
	defer server.Close()
	header := http.Header{"Authorization": []string{"Bearer root-fixture"}}
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/agents/example/exec", header)
	if err != nil {
		t.Fatal("usage wrapper broke WebSocket upgrade", err)
	}
	_, message, err := connection.ReadMessage()
	connection.Close()
	if err != nil || string(message) != "ready" {
		t.Fatal("WebSocket did not survive observation")
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/example", nil)
	request.Header = header
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain") {
		t.Fatal("usage wrapper bypassed safe content sniffing")
	}
}

type usageReadFixture struct {
	*testutil.SecurityAudit
	reads      int
	afterRead  func()
	identities []securityaudit.CredentialUsageIdentity
}

func (s *usageReadFixture) ReadCredentialUsage(_ context.Context, namespace, observer string, identities []securityaudit.CredentialUsageIdentity) (securityaudit.CredentialUsageView, error) {
	s.reads++
	s.identities = identities
	if s.afterRead != nil {
		s.afterRead()
	}
	return securityaudit.CredentialUsageView{Items: []securityaudit.CredentialUsageTotals{}, Coverage: securityaudit.CredentialUsageCoverage{Dropped: "0", Unrecognized: "0", Unattributed: "0", Unavailable: "0"}}, nil
}
func TestManagedCredentialUsageReadChecksCurrentAdminAndNamespace(t *testing.T) {
	observer := newUsageObserverFixture()
	store := &usageReadFixture{SecurityAudit: testutil.NewSecurityAudit()}
	server := &Server{Namespace: "kyber-system", APIKey: "root-fixture", Callers: []ScopedCaller{usageCallerFixture("restricted-fixture", "Reporter")}, SecurityAudit: store, OperatorCredentialUsage: observer}
	handler := server.BuildHandler()
	request := func(key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/operator-credentials/usage", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request("restricted-fixture"); w.Code != http.StatusForbidden || store.reads != 0 {
		t.Fatal("restricted caller read activity")
	}
	store.afterRead = func() { server.auth.SetKey("replacement-fixture") }
	if w := request("root-fixture"); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "coverage") {
		t.Fatal("retired key received protected metadata")
	}
	store.afterRead = nil
	observer.namespace = "other-namespace"
	if w := request("replacement-fixture"); w.Code != http.StatusServiceUnavailable || store.reads != 1 {
		t.Fatal("cross-namespace observer was accepted")
	}
}

func TestManagedCredentialUsagePreservesBrowserStreaming(t *testing.T) {
	observer := newUsageObserverFixture()
	auth := NewAPIKeyAuthenticator("root-fixture")
	token, err := auth.CreateBrowserSession(*installationAdminCaller())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(authMiddleware(auth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: ready\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}), observer))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/agents/secrets/logs", nil)
	request.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: token})
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	response.Body.Close()
	cancel()
	if err != nil || line != "data: ready\n" {
		t.Fatal("usage wrapper broke streaming flush")
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.observations) != 1 || observer.observations[0].Transport != "browser-session" || observer.observations[0].Action != "logs:read" {
		t.Fatal("browser stream was misclassified or counted more than once")
	}
}
func TestManagedCredentialUsageObservesBrowserExchangeOnce(t *testing.T) {
	observer := newUsageObserverFixture()
	server := &Server{Namespace: "kyber-system", APIKey: "root-fixture", OperatorCredentialUsage: observer}
	handler := server.BuildHandler()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/browser-session", nil)
	r.Header.Set("Authorization", "Bearer root-fixture")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || len(observer.observations) != 1 || observer.observations[0].Action != "browser:sign-in" || observer.observations[0].Transport != "bearer" {
		t.Fatal("browser exchange was omitted or counted twice")
	}
}
