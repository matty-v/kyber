//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
	"github.com/matty-v/kyber/pkg/podtoken"
	"github.com/matty-v/kyber/pkg/runtimes"
	"github.com/matty-v/kyber/pkg/taskstore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type processRuntime struct{}

func (processRuntime) Type() string { return "contract-process-fixture" }
func (processRuntime) Adapter() runtimes.Adapter {
	return processAdapter{runtimes.NewStubAdapter("test.invalid/contract-process-fixture:pinned", []string{"python3", "/fixture/contract_harness.py"}, nil, nil, nil, nil, 10, "/persist/brief.json", "/persist/state.json", "FIXTURE_MODEL")}
}
func (processRuntime) Probe() runtimes.Probe { return processProbe{} }
func (processRuntime) Descriptor() runtimes.Descriptor {
	return runtimes.Descriptor{ID: "contract-process-fixture", Name: "Contract process fixture", ContractVersion: runtimes.ContractVersion, Profile: runtimes.InteractiveProfile, Cancellation: "notify_only", AuthModes: []runtimes.AuthMode{{ID: kyberv1.AgentAuthTypeAPIKey, Name: "Fixture key", Flow: "api-key", InputField: "fixtureKey", SecretSuffix: "fixture"}}, Features: []runtimes.Feature{runtimes.TaskReceipts, runtimes.TaskTools}, TranscriptPath: ".fixture", TranscriptExchange: `{role:.role,content:.content,timestamp:""}`}
}
func (processRuntime) Authentication() runtimes.Authentication { return processAuth{} }

type processAdapter struct{ runtimes.Adapter }

func (processAdapter) Type() string { return "contract-process-fixture" }

type processProbe struct{}

func (processProbe) Type() string { return "contract-process-fixture" }

type processAuth struct{}

func (processAuth) Validate(mode kyberv1.AgentAuthType, in runtimes.AuthInput) error {
	if mode != kyberv1.AgentAuthTypeAPIKey || in["fixtureKey"] != "fixture-only-key" {
		return fmt.Errorf("invalid fixture authentication")
	}
	return nil
}
func (processAuth) Prepare(ctx context.Context, mode kyberv1.AgentAuthType, in runtimes.AuthInput, _ runtimes.AuthOptions) ([]runtimes.Credential, error) {
	if err := (processAuth{}).Validate(mode, in); err != nil {
		return nil, err
	}
	return []runtimes.Credential{{Suffix: "fixture", Data: map[string][]byte{"token": []byte(in["fixtureKey"])}}}, nil
}
func (processAuth) Pending(kyberv1.AgentAuthType, map[string][]byte) bool { return false }

// Real tmux + the production paste/receipt scripts + authenticated Internal API
// + PostgreSQL. Kubernetes observations are fixtures; upstream CLIs are tested
// separately. Receipt acceptance must not be confused with task completion.
func TestHarnessContractBootReceiptAndExplicitCompletion(t *testing.T) {
	for _, tool := range []string{"tmux", "python3", "bash", "jq", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("conformance prerequisite %s: %v", tool, err)
		}
	}
	if _, ok := runtimes.Get("contract-process-fixture"); !ok {
		runtimes.Register(processRuntime{})
	}
	rt, _ := runtimes.Get("contract-process-fixture")
	auth, _ := runtimes.AuthenticationFor(rt.Type())
	credentials, err := auth.Prepare(t.Context(), kyberv1.AgentAuthTypeAPIKey, runtimes.AuthInput{"fixtureKey": "fixture-only-key"}, runtimes.AuthOptions{})
	if err != nil {
		t.Fatal(err)
	}
	store := newTaskStore(t)
	owner := taskstore.AgentRef{Namespace: testNamespace, Name: "harness-fixture"}
	taskID := "task_44444444444444444444444444444444"
	attempt := "attempt_55555555555555555555555555555555"
	if _, err := store.Create(t.Context(), taskstore.CreateParams{ID: taskID, Agent: owner, CreatedBy: "operator", Prompt: "fixture prompt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimPending(t.Context(), "fixture-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginAttempt(t.Context(), owner, taskID, "fixture-worker", attempt); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = kyberv1.AddToScheme(scheme)
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace},
		Spec: kyberv1.AgentSpec{
			Runtime: rt.Type(),
		},
	}
	agent.Spec.Secrets.AuthType = kyberv1.AgentAuthTypeAPIKey
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	key := []byte("contract-fixture-signing-key-only-0000")
	internal := httptest.NewServer(api.NewInternalServer(briefstore.NewMemoryStore(), api.WithKubeClient(kube, testNamespace), api.WithTaskStore(store), api.WithInternalAuth(api.NewHMACInternalAuthenticator(key), false)).Handler())
	defer internal.Close()
	// The loopback relay models the status-sidecar's self-scoped credential;
	// the harness process never receives the control-plane token.
	target, _ := url.Parse(internal.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		r.URL.Path = "/internal/agents/" + owner.Name + r.URL.Path
		r.Header.Set("Authorization", "Bearer "+podtoken.Sign(owner.Name, key))
	}
	relay := httptest.NewServer(proxy)
	defer relay.Close()
	response, err := http.Post(internal.URL+"/internal/agents/"+owner.Name+"/task-receipts", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("unauthenticated status=%d", response.StatusCode)
	}
	root := t.TempDir()
	socket := filepath.Join(root, "tmux.sock")
	script, _ := filepath.Abs("fixtures/contract_harness.py")
	receipt, _ := filepath.Abs("../../images/agent-base/scripts/kyber-task-receipt")
	paste, _ := filepath.Abs("../../images/agent-base/scripts/kyber-tmux-paste.sh")
	home := runtimes.TranscriptRoot(rt.Type(), root)
	env := append(os.Environ(), "FIXTURE_HOME="+home, "FIXTURE_KEY="+string(credentials[0].Data["token"]), "FIXTURE_RECEIPT_HOOK="+receipt, "KYBER_TASK_RECEIPT_URL="+relay.URL+"/task-receipts", "KYBER_TASK_RECEIPT_DIR="+filepath.Join(root, "receipts"), "FIXTURE_COMPLETE_URL="+relay.URL+"/task-complete")
	tmux := func(args ...string) {
		t.Helper()
		cmd := exec.Command("tmux", append([]string{"-S", socket}, args...)...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture tmux: %v %s", err, out)
		}
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	tmux("new-session", "-d", "-s", "agent", "python3", script)
	waitFile := func(name string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(home, name)); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("fixture did not reach %s", name)
	}
	waitFile("ready")
	prompt := "[kyber-task:" + taskID + "] attempt=" + attempt + "\nfixture prompt"
	delivery := exec.Command("bash", "-c", `tmux() { command tmux -S "$FIXTURE_SOCKET" "$@"; }; source "$FIXTURE_PASTE"; kyber_tmux_paste agent fixture-buffer "$FIXTURE_PROMPT"`)
	delivery.Env = append(os.Environ(), "FIXTURE_SOCKET="+socket, "FIXTURE_PASTE="+paste, "FIXTURE_PROMPT="+prompt)
	if out, err := delivery.CombinedOutput(); err != nil {
		t.Fatalf("paste: %v %s", err, out)
	}
	waitFile("receipt-ready")
	observed, err := store.Get(t.Context(), owner, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.State != taskstore.StateDispatched {
		t.Fatalf("receipt incorrectly finalized task: %s", observed.State)
	}
	durableReceipt, err := store.GetReceipt(t.Context(), owner, attempt)
	if err != nil || durableReceipt.Runtime != rt.Type() {
		t.Fatalf("receipt not durable: %v", err)
	}
	transcript, err := os.ReadFile(filepath.Join(home, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var exchange map[string]string
	if json.Unmarshal(transcript, &exchange) != nil || exchange["content"] != prompt {
		t.Fatal("transcript lost delivered prompt")
	}
	if err := os.WriteFile(filepath.Join(home, "complete-now"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitFile("completed")
	observed, err = store.Get(t.Context(), owner, taskID)
	if err != nil || observed.State != taskstore.StateCompleted || observed.Response != "fixture-complete" {
		t.Fatalf("explicit completion missing: %v", err)
	}
	tmux("kill-session", "-t", "agent")
	waitFile("shutdown")
}
