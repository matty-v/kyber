//go:build integration

package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/internal/testutil"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/archivejob"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Uses only disposable registry/marker Secrets. HTTP handlers run in process
// with real direct authority reads; deployed authentication is never activated.
func TestGCPDevRegistryRequestResolution(t *testing.T) {
	if os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_VERIFY") != "1" {
		t.Skip("explicit GCP dev verification required")
	}
	const namespace = "kyber-system"
	const contextName = "gke_datawire-dev_us-central1-a_kyber-dev"
	expectedHost := os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_HOST")
	if expectedHost == "" || os.Getenv("KUBECONFIG") == "" {
		t.Fatal("verified dev endpoint/configuration required")
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: os.Getenv("KUBECONFIG")}, &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	if err != nil || config.Host != expectedHost {
		t.Fatal("dev context/endpoint mismatch")
	}
	config.Timeout = 5 * time.Second
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, kyberv1.AddToScheme} {
		if add(scheme) != nil {
			t.Fatal("fixture scheme failed")
		}
	}
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal("dev client unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var deployment appsv1.Deployment
	if err := direct.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "kyber-dev-control-plane"}, &deployment); err != nil || deployment.Labels["app.kubernetes.io/instance"] != "kyber-dev" {
		t.Fatal("dev installation mismatch")
	}
	var agents kyberv1.AgentList
	if err := direct.List(ctx, &agents, client.InNamespace(namespace)); err != nil || len(agents.Items) != 0 {
		t.Fatal("dev Agent preflight failed")
	}
	readMetadata := func(name string) *metav1.PartialObjectMetadata {
		metadata := &metav1.PartialObjectMetadata{}
		metadata.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
		if err := direct.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, metadata); err != nil {
			t.Fatal("fixture metadata read failed")
		}
		return metadata
	}
	rootBefore := readMetadata("kyber-api-credentials")
	name := "operator-auth-probe-" + uuid.NewString()
	markerName := name + "-authority"
	owned := map[string]types.UID{}
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		for _, objectName := range []string{markerName, name} {
			secret := &corev1.Secret{}
			key := client.ObjectKey{Namespace: namespace, Name: objectName}
			if err := direct.Get(cleanupCtx, key, secret); apierrors.IsNotFound(err) {
				continue
			} else if err != nil {
				t.Error("probe cleanup read failed")
				continue
			}
			if owned[objectName] == "" || secret.UID != owned[objectName] || secret.Labels["app.kubernetes.io/managed-by"] != "kyber" {
				t.Error("probe cleanup ownership mismatch")
				continue
			}
			uid := secret.UID
			if err := direct.Delete(cleanupCtx, secret, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
				t.Error("probe cleanup delete failed")
				continue
			}
			if err := direct.Get(cleanupCtx, key, &corev1.Secret{}); !apierrors.IsNotFound(err) {
				t.Error("probe remains after cleanup")
			}
		}
	})
	store, err := operatorcredentials.NewSecretStore(config, namespace, name)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := operatorcredentials.NewAuthorityStore(config, namespace, name, markerName)
	if err != nil {
		t.Fatal(err)
	}
	authority2, err := operatorcredentials.NewAuthorityStore(config, namespace, name, markerName)
	if err != nil {
		t.Fatal(err)
	}
	document := registryFixtureDocument(t)
	operation := func(action, target string) operatorcredentials.Operation {
		return operatorcredentials.Operation{ID: uuid.NewString(), AdmissionEventID: uuid.NewString(), CompletionEventID: uuid.NewString(), RequestID: uuid.NewString(), Action: action, ActorPrincipalID: "other-principal", ActorCredentialID: "other-admin", ActorGeneration: 1, Authority: "registry", TargetCredentialID: target, At: time.Now().UTC(), Status: "pending"}
	}
	document.Credentials[2].Scopes = append(document.Credentials[2].Scopes, string(ScopeArchivesAdmin))
	document.Operations = []operatorcredentials.Operation{operation("adopt", "")}
	created, err := store.Create(ctx, document)
	if err != nil {
		t.Fatal("probe registry create failed")
	}
	owned[name] = readMetadata(name).UID
	server := func(source OperatorCredentialAuthority) *Server {
		return &Server{Namespace: namespace, APIKey: "original-installation-fixture", OperatorCredentialAuthority: source, Archives: &ArchiveService{SigningKey: []byte("private-archive-probe-fixture")}, Callers: []ScopedCaller{{Name: "configuration-only", Key: "configuration-only-fixture"}}}
	}
	a, b := server(authority), server(authority2)
	handlers := []http.Handler{a.BuildHandler(), b.BuildHandler()}
	check := func(handler http.Handler, request *http.Request, want int) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("fixture response status=%d want=%d", response.Code, want)
		}
	}
	for _, handler := range handlers {
		check(handler, registryBearer("original-installation-fixture"), 200)
		check(handler, registryBearer("configuration-only-fixture"), 200)
		check(handler, registryBearer("other-admin-fixture"), 401)
	}
	oldRoot, err := a.auth.CreateBrowserSession(*installationAdminCaller())
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := authority.ReserveAdoption(ctx)
	if err != nil {
		t.Fatal("probe maintenance reservation failed")
	}
	t.Cleanup(func() {
		if err := reservation.Release(context.Background()); err != nil {
			t.Error("probe reservation cleanup failed")
		}
	})
	if err := authority.Bind(ctx, created.Version, reservation); err != nil {
		t.Fatal("probe authority binding failed")
	}
	owned[markerName] = readMetadata(markerName).UID
	for _, handler := range handlers {
		check(handler, registryCookie(oldRoot), 200)
		check(handler, registryBearer("original-installation-fixture"), 200)
		check(handler, registryBearer("configuration-only-fixture"), 401)
		check(handler, registryBearer("other-admin-fixture"), 200)
	}
	scoped := mustRegistryCaller(t, a.auth, registryBearer("client-key-fixture"))
	cookie := mustRegistryCookie(t, a.auth, *scoped)
	archiveRequest := registryCookie(cookie)
	archiveRequest = archiveRequest.WithContext(context.WithValue(ctx, callerCtxKey{}, scoped))
	archiveToken, err := a.signCurrentArchiveDownload(archiveRequest, &archivejob.Job{ID: "archive-probe-job", Agent: "alpha"}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal("probe archive capability issuance failed")
	}
	archiveClaims, ok := a.parseCurrentArchiveDownload(archiveToken)
	if !ok {
		t.Fatal("probe archive capability parse failed")
	}
	if _, err := b.resolveArchiveDownload(ctx, archiveClaims); err != nil {
		t.Fatal("independent replica rejected archive capability")
	}
	binding, err := browserSessionKeyBinding("original-installation-fixture", "client-key-fixture")
	if err != nil {
		t.Fatal(err)
	}
	oldClient, err := signBrowserSession("original-installation-fixture", *scoped, binding, time.Now(), browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	next := created.Document.Clone()
	rootCredential := registryFixtureCredential(t, &next, installationAdminCredentialID)
	*rootCredential = rootCredential.WithSecretMaterial([]byte("rotated-installation-fixture"))
	rootCredential.Generation++
	next.Operations = append(next.Operations, operation("rotate", installationAdminCredentialID))
	rotated, err := store.Replace(ctx, created.Version, next)
	if err != nil {
		t.Fatal("probe individual rotation failed")
	}
	if _, err := b.resolveArchiveDownload(ctx, archiveClaims); err != nil {
		t.Fatal("unrelated rotation invalidated archive capability")
	}
	forgedRoot := *installationAdminCaller()
	forgedRoot.CredentialGeneration = 2
	forgedCookie, err := signBrowserSession("original-installation-fixture", forgedRoot, "", time.Now(), browserSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	for _, handler := range handlers {
		check(handler, registryCookie(oldRoot), 401)
		check(handler, registryCookie(forgedCookie), 401)
		check(handler, registryBearer("original-installation-fixture"), 401)
		check(handler, registryCookie(oldClient), 200)
		check(handler, registryCookie(cookie), 200)
	}
	// A newly constructed server/authority simulates a fresh replica, retaining
	// all accepted identities/cookies without an in-memory credential snapshot.
	freshSource, err := operatorcredentials.NewAuthorityStore(config, namespace, name, markerName)
	if err != nil {
		t.Fatal(err)
	}
	handlers = append(handlers, server(freshSource).BuildHandler())
	// Verify a live HTTP stream against the same real GCP authority. No real
	// Agent or runtime is created; the protected backend is a blocked fixture.
	liveStopped := make(chan struct{})
	liveService := &Server{Namespace: namespace, SecurityAudit: testutil.NewSecurityAudit(), auth: a.auth}
	liveServer := httptest.NewServer(authMiddleware(a.auth, liveService.auditSensitiveRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !liveService.requireSensitiveResource(w, r, "alpha", ScopeLogsRead) {
			return
		}
		_, _ = io.WriteString(w, "ready\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(liveStopped)
	}))))
	defer liveServer.Close()
	liveRequest, _ := http.NewRequest(http.MethodGet, liveServer.URL+"/api/v1/agents/alpha/logs", nil)
	liveRequest.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: cookie})
	liveResponse, err := liveServer.Client().Do(liveRequest)
	if err != nil || liveResponse.StatusCode != 200 {
		t.Fatal("live authority stream did not establish")
	}
	defer liveResponse.Body.Close()
	ready := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(liveResponse.Body, ready); err != nil {
		t.Fatal("live stream did not flush")
	}
	next = rotated.Document.Clone()
	clientCredential := registryFixtureCredential(t, &next, scoped.CredentialID)
	clientCredential.Scopes = []string{"tasks:read"}
	clientCredential.PermissionRevision++
	next.Operations = append(next.Operations, operation("update", clientCredential.ID))
	narrowed, err := store.Replace(ctx, rotated.Version, next)
	if err != nil {
		t.Fatal("probe grant edit failed")
	}
	if _, err := b.resolveArchiveDownload(ctx, archiveClaims); err == nil {
		t.Fatal("archive capability retained removed permission")
	}
	select {
	case <-liveStopped:
	case <-time.After(10 * time.Second):
		t.Fatal("real authority grant narrowing did not close live access")
	}
	_, _ = io.Copy(io.Discard, liveResponse.Body)

	for _, auth := range []*APIKeyAuthenticator{a.auth, b.auth} {
		current := mustRegistryCaller(t, auth, registryCookie(cookie))
		if current.Scopes.Has(ScopeMetricsRead) || current.PrincipalID != scoped.PrincipalID || current.TenantID != scoped.TenantID {
			t.Fatal("old permissions or changed task ownership")
		}
	}
	next = narrowed.Document.Clone()
	clientCredential = registryFixtureCredential(t, &next, scoped.CredentialID)
	*clientCredential = clientCredential.WithSecretMaterial(nil)
	clientCredential.State = "revoked"
	clientCredential.PermissionRevision++
	next.Operations = append(next.Operations, operation("revoke", clientCredential.ID))
	if _, err := store.Replace(ctx, narrowed.Version, next); err != nil {
		t.Fatal("probe revocation failed")
	}
	for _, handler := range handlers {
		check(handler, registryBearer("client-key-fixture"), 401)
		check(handler, registryCookie(cookie), 401)
		check(handler, registryCookie(oldClient), 401)
		check(handler, registryBearer("other-admin-fixture"), 200)
	}
	// Missing adopted registry is a 503 across replicas, never a fake 401 or
	// restored bootstrap key. Delete only the inert Secret with our recorded UID.
	registryUID := owned[name]
	if err := direct.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &registryUID}}); err != nil {
		t.Fatal("inert outage fixture deletion failed")
	}
	for _, handler := range handlers {
		check(handler, registryBearer("original-installation-fixture"), 503)
		check(handler, registryCookie(cookie), 503)
	}
	if _, err := b.resolveArchiveDownload(ctx, archiveClaims); !errors.Is(err, errOperatorAuthorityUnavailable) {
		t.Fatal("archive authority outage misreported")
	}
	if _, err := freshSource.Load(ctx); !errors.Is(err, operatorcredentials.ErrUnavailable) {
		t.Fatal("missing authority became bootstrap")
	}
	rootAfter := readMetadata("kyber-api-credentials")
	if rootBefore.UID != rootAfter.UID || rootBefore.ResourceVersion != rootAfter.ResourceVersion {
		t.Fatal("live credential metadata changed")
	}
	if err := direct.List(ctx, &agents, client.InNamespace(namespace)); err != nil || len(agents.Items) != 0 {
		t.Fatal("dev Agent postflight failed")
	}
	// A bounded safe diagnostic, with no handler bodies, keys, cookies or seed.
	t.Log(strings.Join([]string{"Real dev authority reads: adoption, V1/V2 compatibility, independent replica resolution, isolated rotation, current grants, bounded live-stream cancellation, archive capability resolution/narrowing, revocation and 503 outage passed.", "Live credential metadata unchanged; zero Agents; owned probe cleanup verified."}, " "))
}

func registryFixtureCredential(t *testing.T, document *operatorcredentials.Document, id string) *operatorcredentials.Credential {
	t.Helper()
	for i := range document.Credentials {
		if document.Credentials[i].ID == id {
			return &document.Credentials[i]
		}
	}
	t.Fatal("fixture credential missing")
	return nil
}
