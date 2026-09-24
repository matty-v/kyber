package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	utilexec "k8s.io/client-go/util/exec"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
)

type recordedDispatch struct {
	pod   string
	args  []string
	stdin string
}

func runNowServer(t *testing.T, job kyberv1.AgentJob, stdout string, err error) (*Server, *recordedDispatch) {
	t.Helper()
	scheme := runtime.NewScheme()
	if e := clientgoscheme.AddToScheme(scheme); e != nil {
		t.Fatal(e)
	}
	if e := kyberv1.AddToScheme(scheme); e != nil {
		t.Fatal(e)
	}
	agent := &kyberv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "chewie", Namespace: "kyber-system"},
		Spec:       kyberv1.AgentSpec{Runtime: "claude-code", Jobs: []kyberv1.AgentJob{job}},
	}
	rec := &recordedDispatch{}
	s := &Server{
		K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build(),
		Namespace: "kyber-system",
		jobDispatchExec: func(_ context.Context, pod string, args []string, stdin io.Reader) (string, string, error) {
			body, _ := io.ReadAll(stdin)
			*rec = recordedDispatch{pod: pod, args: args, stdin: string(body)}
			return stdout, "", err
		},
	}
	return s, rec
}

func postRunNow(s *Server, job string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	s.handleAgentJobAction(rr, httptest.NewRequest(http.MethodPost, "/api/v1/agents/chewie/jobs/"+job+"/run", nil), "chewie", job+"/run")
	return rr
}

// MAT-90 G10: run-now sends the prompt from spec.jobs on stdin (the projected
// prompt file lags a just-saved job) and carries the job's own flags.
func TestRunNowSendsSpecPromptAndFlags(t *testing.T) {
	job := kyberv1.AgentJob{Name: "weekly", Schedule: "0 3 * * 0", Prompt: "summarize the week", Exclusive: true, ClearContextAfter: true}
	s, rec := runNowServer(t, job, "kyber-job-dispatch: outcome=success reason=none\n", nil)
	rr := postRunNow(s, "weekly")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	want := "--run-now --exclusive --clear-context-after weekly"
	if got := strings.Join(rec.args, " "); got != want || rec.pod != "agent-chewie" {
		t.Fatalf("dispatch pod=%q args=%q, want agent-chewie %q", rec.pod, got, want)
	}
	if rec.stdin != "summarize the week" {
		t.Fatalf("stdin=%q, want the spec prompt", rec.stdin)
	}
}

func TestRunNowReportsDispatchOutcome(t *testing.T) {
	job := kyberv1.AgentJob{Name: "tick", Schedule: "* * * * *", Prompt: "tick", Exclusive: true}
	for _, tc := range []struct {
		name   string
		code   int
		stdout string
		want   int
		reason string
	}{
		{"skipped", 5, "kyber-job-dispatch: outcome=skipped reason=agent_busy\n", http.StatusConflict, "agent_busy"},
		{"failed", 3, "kyber-job-dispatch: outcome=failed reason=tmux_session_absent\n", http.StatusServiceUnavailable, "tmux_session_absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := runNowServer(t, job, tc.stdout, utilexec.CodeExitError{Err: errors.New("exit"), Code: tc.code})
			rr := postRunNow(s, "tick")
			if rr.Code != tc.want || !strings.Contains(rr.Body.String(), tc.reason) {
				t.Fatalf("status=%d body=%s, want %d naming %s", rr.Code, rr.Body.String(), tc.want, tc.reason)
			}
		})
	}
}

// An agent image from before --run-now rejects the flag with a usage error;
// run-now falls back to the --stdin form that image understands.
func TestRunNowFallsBackForOlderAgentImages(t *testing.T) {
	job := kyberv1.AgentJob{Name: "weekly", Schedule: "0 3 * * 0", Prompt: "summarize", Exclusive: true}
	s, _ := runNowServer(t, job, "", nil)
	var calls [][]string
	var stdins []string
	s.jobDispatchExec = func(_ context.Context, _ string, args []string, stdin io.Reader) (string, string, error) {
		body, _ := io.ReadAll(stdin)
		calls, stdins = append(calls, append([]string(nil), args...)), append(stdins, string(body))
		if args[0] == "--run-now" {
			return "", "kyber-job-dispatch: unknown flag --run-now\n", utilexec.CodeExitError{Err: errors.New("exit"), Code: 2}
		}
		return "", "", nil
	}
	rr := postRunNow(s, "weekly")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(calls) != 2 || strings.Join(calls[1], " ") != "--stdin --exclusive weekly" || stdins[1] != "summarize" {
		t.Fatalf("calls=%q stdins=%q", calls, stdins)
	}
}
