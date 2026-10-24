//go:build integration

package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
	"github.com/matty-v/kyber/pkg/securityaudit"
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

// Real Kubernetes and PostgreSQL, but only disposable authority and DB schema.
// No installation key, Deployment, Agent or default authority is changed.
func TestGCPDevManagedCredentialLifecycle(t *testing.T) {
	if os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_VERIFY") != "1" || os.Getenv("KYBER_PG_DSN") == "" {
		t.Skip("explicit dev/database verification required")
	}
	const namespace = "kyber-system"
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: os.Getenv("KUBECONFIG")}, &clientcmd.ConfigOverrides{CurrentContext: "gke_datawire-dev_us-central1-a_kyber-dev"}).ClientConfig()
	if err != nil || os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_HOST") == "" || config.Host != os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_HOST") {
		t.Fatal("dev context/endpoint mismatch")
	}
	config.Timeout = 5 * time.Second
	scheme := runtime.NewScheme()
	_ = kyberv1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal("dev client unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var deployment appsv1.Deployment
	if direct.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "kyber-dev-control-plane"}, &deployment) != nil || deployment.Labels["app.kubernetes.io/instance"] != "kyber-dev" {
		t.Fatal("dev installation mismatch")
	}
	checkEmpty := func() {
		t.Helper()
		var agents kyberv1.AgentList
		if direct.List(ctx, &agents, client.InNamespace(namespace)) != nil || len(agents.Items) != 0 {
			t.Fatal("dev Agent preflight/postflight failed")
		}
	}
	checkEmpty()
	metadata := func(name string) *metav1.PartialObjectMetadata {
		t.Helper()
		result := &metav1.PartialObjectMetadata{}
		result.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
		if direct.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, result) != nil {
			t.Fatal("credential metadata unavailable")
		}
		return result
	}
	rootBefore := metadata("kyber-api-credentials")
	audit := isolatedCredentialManagementAudit(t, ctx)
	name := "credential-management-probe-" + uuid.NewString()
	markerName := name + "-authority"
	store, err := operatorcredentials.NewSecretStore(config, namespace, name)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := operatorcredentials.NewAuthorityStore(config, namespace, name, markerName)
	if err != nil {
		t.Fatal(err)
	}
	owned := map[string]types.UID{}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		for _, objectName := range []string{markerName, name} {
			secret := &corev1.Secret{}
			key := client.ObjectKey{Namespace: namespace, Name: objectName}
			if err := direct.Get(cleanup, key, secret); apierrors.IsNotFound(err) {
				continue
			} else if err != nil {
				t.Error("probe cleanup read failed")
				continue
			}
			if owned[objectName] == "" || owned[objectName] != secret.UID || secret.Labels["app.kubernetes.io/managed-by"] != "kyber" {
				t.Error("probe cleanup ownership mismatch")
				continue
			}
			uid := secret.UID
			if direct.Delete(cleanup, secret, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}) != nil {
				t.Error("probe cleanup delete failed")
				continue
			}
			if err := direct.Get(cleanup, key, &corev1.Secret{}); !apierrors.IsNotFound(err) {
				t.Error("probe cleanup not verified")
			}
		}
	})
	document := registryFixtureDocument(t)
	document.Operations = []operatorcredentials.Operation{adoptionFixtureOperation()}
	created, err := store.Create(ctx, document)
	if err != nil || created.UID == "" {
		t.Fatal("owned registry creation failed")
	}
	owned[name] = types.UID(created.UID)
	reservation, err := authority.ReserveAdoption(ctx)
	if err != nil {
		t.Fatal("probe reservation failed")
	}
	t.Cleanup(func() {
		if reservation.Release(context.Background()) != nil {
			t.Error("probe reservation cleanup failed")
		}
	})
	if authority.Bind(ctx, created.Version, reservation) != nil {
		t.Fatal("probe authority binding failed")
	}
	owned[markerName] = metadata(markerName).UID
	server := &Server{Namespace: namespace, APIKey: "original-installation-fixture", OperatorCredentialAuthority: authority, SecurityAudit: audit}
	handler := server.BuildHandler()
	reviewedVersion := func() string {
		t.Helper()
		r := registryBearer("original-installation-fixture")
		r.URL.Path = "/api/v1/operator-credentials"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		var inventory operatorCredentialInventory
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &inventory) != nil || !inventory.ManagementAvailable || inventory.Version == "" {
			t.Fatal("real managed inventory unavailable")
		}
		return inventory.Version
	}
	post := func(request credentialMutationRequest) credentialOperationView {
		t.Helper()
		response := managedMutationHTTP(t, handler, request, reviewedVersion(), "original-installation-fixture")
		var result credentialOperationView
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Status != "success" || strings.Contains(response.Body.String(), request.Key) && request.Key != "" {
			t.Fatal("real managed key change failed or exposed material")
		}
		operation, err := audit.GetCredentialOperation(ctx, namespace, request.OperationID)
		if err != nil || operation.Completion == nil || operation.Completion.Outcome != "success" {
			t.Fatal("real completion not durable")
		}
		return result
	}
	optionsRequest := registryBearer("original-installation-fixture")
	optionsRequest.URL.Path = "/api/v1/operator-credentials/options"
	optionsResponse := httptest.NewRecorder()
	handler.ServeHTTP(optionsResponse, optionsRequest)
	var choices operatorCredentialOptions
	if optionsResponse.Code != http.StatusOK || json.Unmarshal(optionsResponse.Body.Bytes(), &choices) != nil || choices.Version != 1 || choices.Namespace != namespace || len(choices.Permissions) != len(managedPermissionLabels) {
		t.Fatal("real managed permission choices unavailable")
	}
	staleVersion := reviewedVersion()
	request := managedCreateFixtureRequest()
	result := post(request)
	credentialID := result.CredentialID
	original, err := server.auth.Authenticate(registryBearer(request.Key))
	if err != nil {
		t.Fatal("created key not accepted")
	}
	update := credentialMutationRequest{OperationID: uuid.NewString(), Action: "update", CredentialID: credentialID, Policy: &credentialMutationPolicy{Name: "Updated integration", Role: operatorcredentials.RoleRestricted, Scopes: []string{string(ScopeMetricsRead)}}}
	post(update)
	changed, err := server.auth.Authenticate(registryBearer(request.Key))
	if err != nil || !sameCredentialIdentity(*original, *changed) || changed.Scopes.Has(ScopeTasksRead) || !changed.Scopes.Has(ScopeMetricsRead) {
		t.Fatal("real update changed ownership or retained old grants")
	}
	rotatedKey := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("+", 32)))
	post(credentialMutationRequest{OperationID: uuid.NewString(), Action: "rotate", CredentialID: credentialID, Key: rotatedKey})
	if _, err := server.auth.Authenticate(registryBearer(request.Key)); err == nil {
		t.Fatal("old key survived real rotation")
	}
	rotated, err := server.auth.Authenticate(registryBearer(rotatedKey))
	if err != nil || rotated.PrincipalID != original.PrincipalID || rotated.TenantID != original.TenantID || rotated.CredentialGeneration != original.CredentialGeneration+1 {
		t.Fatal("real rotation changed ownership or generation")
	}
	post(credentialMutationRequest{OperationID: uuid.NewString(), Action: "revoke", CredentialID: credentialID})
	if _, err := server.auth.Authenticate(registryBearer(rotatedKey)); err == nil {
		t.Fatal("revoked key accepted")
	}
	beforeRejected, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rejected := managedCreateFixtureRequest()
	rejected.Policy.Name = "Not created"
	if response := managedMutationHTTP(t, handler, rejected, staleVersion, "original-installation-fixture"); response.Code != http.StatusConflict {
		t.Fatal("stale real review accepted")
	}
	afterRejected, err := store.Load(ctx)
	if err != nil || afterRejected.Version != beforeRejected.Version {
		t.Fatal("stale request wrote registry")
	}
	for _, action := range []string{"credential:create", "credential:update", "credential:rotate", "credential:revoke"} {
		page, err := audit.Query(ctx, securityaudit.Filter{Namespace: namespace, Action: action, Limit: 10}, nil)
		if err != nil || len(page.Items) != 2 {
			t.Fatal("real required admission/completion pair missing")
		}
	}
	// Simulate a crash after durable admission but before any key write.
	snapshot, err := authority.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := server.auth.Authenticate(registryBearer("original-installation-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	interrupted := managedCreateFixtureRequest()
	interrupted.Policy.Name = "Interrupted integration"
	interrupted.Key = base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(",", 32)))
	late, reference, err := prepareManagedCredentialMutation(snapshot, *actor, interrupted, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	intent := securityaudit.CredentialIntent{Admission: securityaudit.Event{ID: reference.AdmissionEventID, OperationID: reference.ID, ObservedAt: reference.At, Namespace: namespace, PrincipalID: reference.ActorPrincipalID, CredentialID: reference.ActorCredentialID, Generation: reference.ActorGeneration, CredentialAuthority: "registry", RequestID: reference.RequestID, Action: "credential:create", ResourceKind: "credential", ResourceName: reference.TargetCredentialID, Phase: "authorization", Outcome: "allow"}, CompletionEventID: reference.CompletionEventID, ExpectedVersion: snapshot.Version, RegistryUID: snapshot.UID}
	if _, err := audit.AdmitCredentialOperation(ctx, intent); err != nil {
		t.Fatal("interrupted intent admission failed")
	}
	pendingRequest := registryBearer("original-installation-fixture")
	pendingRequest.URL.Path = "/api/v1/operator-credential-operations/pending"
	pendingResponse := httptest.NewRecorder()
	handler.ServeHTTP(pendingResponse, pendingRequest)
	var pending operatorCredentialPendingPage
	if pendingResponse.Code != http.StatusOK || json.Unmarshal(pendingResponse.Body.Bytes(), &pending) != nil || len(pending.Items) != 1 || pending.Items[0].OperationID != reference.ID {
		t.Fatal("real pending journal operation not visible")
	}
	recovery := reconcileMutationHTTP(t, handler, reference.ID)
	var recovered credentialOperationView
	if recovery.Code != http.StatusOK || json.Unmarshal(recovery.Body.Bytes(), &recovered) != nil || recovered.Status != "failure" {
		t.Fatal("real interrupted write recovery failed")
	}
	if _, err := authority.Replace(ctx, snapshot.UID, snapshot.Version, late); !errors.Is(err, operatorcredentials.ErrConflict) {
		t.Fatal("late write survived real recovery fence")
	}
	if _, err := server.auth.Authenticate(registryBearer(interrupted.Key)); err == nil {
		t.Fatal("interrupted key was issued")
	}
	// Refresh only this browser's same-identity session after acknowledged
	// self-rotation of the disposable registry's installation key.
	resolved, err := server.auth.resolve(ctx)
	if err != nil || resolved.registry == nil {
		t.Fatal("probe registry unavailable")
	}
	priorCookie, err := resolved.registry.createBrowserSession(*actor, time.Now())
	if err != nil {
		t.Fatal("probe browser session unavailable")
	}
	newValue := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("-", 32)))
	body, err := json.Marshal(map[string]string{"operationId": uuid.NewString(), "action": "rotate", "credentialId": actor.CredentialID, "key": newValue})
	if err != nil {
		t.Fatal("probe request encoding failed")
	}
	browserRequest := httptest.NewRequest(http.MethodPost, "/api/v1/operator-credentials/mutations", strings.NewReader(string(body)))
	browserRequest.Header.Set("If-Match", reviewedVersion())
	browserRequest.Header.Set("Origin", "http://example.com")
	browserRequest.AddCookie(&http.Cookie{Name: browserSessionCookie, Value: priorCookie})
	browserResponse := httptest.NewRecorder()
	handler.ServeHTTP(browserResponse, browserRequest)
	cookies := browserResponse.Result().Cookies()
	if browserResponse.Code != http.StatusOK || len(cookies) != 1 {
		t.Fatal("real browser self-rotation did not refresh session")
	}
	currentBrowser, err := server.auth.Authenticate(registryCookie(cookies[0].Value))
	if err != nil || currentBrowser.PrincipalID != actor.PrincipalID || currentBrowser.TenantID != actor.TenantID || currentBrowser.CredentialID != actor.CredentialID || currentBrowser.CredentialGeneration != actor.CredentialGeneration+1 {
		t.Fatal("real browser rotation changed identity", err)
	}
	if _, err := server.auth.Authenticate(registryCookie(priorCookie)); err == nil {
		t.Fatal("old browser session survived self-rotation")
	}
	rootAfter := metadata("kyber-api-credentials")
	if rootBefore.UID != rootAfter.UID || rootBefore.ResourceVersion != rootAfter.ResourceVersion {
		t.Fatal("live credential metadata changed")
	}
	checkEmpty()
	t.Log("Real dev choices, pending recovery, same-identity browser rotation, create/edit/rotate/revoke and durable audit passed; interrupted-write recovery fenced the original version without issuance. Live credentials unchanged; zero Agents; owned probe/schema cleanup verified.")
}

func isolatedCredentialManagementAudit(t *testing.T, ctx context.Context) *securityaudit.PostgresStore {
	t.Helper()
	dsn, err := url.Parse(os.Getenv("KYBER_PG_DSN"))
	if err != nil || dsn.Hostname() != "127.0.0.1" || dsn.Path != "/kyber" {
		t.Fatal("owned local database forward required")
	}
	root, err := sql.Open("postgres", dsn.String())
	if err != nil {
		t.Fatal("database open failed")
	}
	schemaName := "credential_management_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		defer root.Close()
		cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		if _, err := root.ExecContext(cleanup, "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE"); err != nil {
			t.Error("owned schema cleanup failed")
			return
		}
		var remains int
		if root.QueryRowContext(cleanup, "SELECT count(*) FROM information_schema.schemata WHERE schema_name=$1", schemaName).Scan(&remains) != nil || remains != 0 {
			t.Error("owned schema cleanup not verified")
		}
	})
	if _, err := root.ExecContext(ctx, "CREATE SCHEMA "+schemaName); err != nil {
		t.Fatal("isolated schema create failed")
	}
	query := dsn.Query()
	query.Set("search_path", schemaName)
	dsn.RawQuery = query.Encode()
	pool, err := sql.Open("postgres", dsn.String())
	if err != nil {
		t.Fatal("isolated pool open failed")
	}
	t.Cleanup(func() { pool.Close() })
	audit, err := securityaudit.NewPostgresStore(pool)
	if err != nil || audit.Migrate(ctx) != nil {
		t.Fatal("isolated audit schema unavailable")
	}
	return audit
}
