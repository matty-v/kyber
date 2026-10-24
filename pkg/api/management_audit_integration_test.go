//go:build integration

package api

import (
	"context"
	"database/sql"
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
	"github.com/matty-v/kyber/pkg/securityaudit"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This combines direct dev-cluster reads with an isolated dev database schema.
// It never creates an Agent or writes installation credential/configuration data.
func TestGCPDevManagementAudit(t *testing.T) {
	if os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_VERIFY") != "1" || os.Getenv("KYBER_PG_DSN") == "" {
		t.Skip("explicit dev/database verification required")
	}
	const namespace = "kyber-system"
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: os.Getenv("KUBECONFIG")}, &clientcmd.ConfigOverrides{CurrentContext: "gke_datawire-dev_us-central1-a_kyber-dev"}).ClientConfig()
	if err != nil || os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_HOST") == "" || config.Host != os.Getenv("KYBER_OPERATOR_CREDENTIALS_DEV_HOST") {
		t.Fatal("dev context/endpoint mismatch")
	}
	scheme := runtime.NewScheme()
	_ = kyberv1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal("dev client unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	deployment := &appsv1.Deployment{}
	if direct.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "kyber-dev-control-plane"}, deployment) != nil || deployment.Labels["app.kubernetes.io/instance"] != "kyber-dev" {
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
	dsn, err := url.Parse(os.Getenv("KYBER_PG_DSN"))
	if err != nil || dsn.Hostname() != "127.0.0.1" || dsn.Path != "/kyber" {
		t.Fatal("owned local database forward required")
	}
	root, err := sql.Open("postgres", dsn.String())
	if err != nil {
		t.Fatal("database open failed")
	}
	schemaName := "management_audit_probe_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if err != nil {
		t.Fatal("isolated audit store construction failed")
	}
	if audit.Migrate(ctx) != nil {
		t.Fatal("isolated audit schema unavailable")
	}
	server := &Server{Namespace: namespace, APIKey: "management-audit-probe-fixture", SecurityAudit: audit, K8sClient: client.NewDryRunClient(direct)}
	handler := server.BuildHandler()
	missing := "audit-probe-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	request := func() *http.Request {
		r := httptest.NewRequest("POST", "/api/v1/agents/"+missing+"/stop", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer management-audit-probe-fixture")
		return r
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request())
	if response.Code != http.StatusNotFound {
		t.Fatal("missing dev Agent request did not fail safely", response.Code)
	}
	page, err := audit.Query(ctx, securityaudit.Filter{Namespace: namespace, Limit: 10}, nil)
	if err != nil || len(page.Items) != 2 {
		t.Fatal("real database did not persist admission and outcome")
	}
	first, last := page.Items[1], page.Items[0]
	if first.Phase != "authorization" || first.Outcome != "allow" || last.Phase != "completion" || last.Outcome != "failure" || first.OperationID != last.OperationID || first.ResourceName != missing || first.Action != "lifecycle:write" {
		t.Fatal("real database audit metadata mismatch")
	}
	// A closed fixture pool simulates unavailable persistence without disrupting
	// the shared PostgreSQL service. Nil Kubernetes would panic if reached.
	pool.Close()
	server.K8sClient = nil
	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, request())
	if blocked.Code != http.StatusServiceUnavailable || !strings.Contains(blocked.Body.String(), "security_audit_unavailable") {
		t.Fatal("unavailable audit did not block before backend", blocked.Code)
	}
	checkEmpty()
	t.Log("Real dev API read and isolated PostgreSQL admission/completion passed; unavailable persistence blocked before backend; zero Agents and owned schema cleanup verified.")
}
