package api

import (
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/runtimes"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
)

// The canonical auth route dispatches by registered flow. Legacy /oauth and
// /codex-device-auth remain aliases with their original request/response shapes.
func (s *Server) handleRuntimeReauthorize(w http.ResponseWriter, r *http.Request, name string) {
	agent := &kyberv1.Agent{}
	if err := s.K8sClient.Get(r.Context(), types.NamespacedName{Name: name, Namespace: s.Namespace}, agent); err != nil {
		writeJSONError(w, 404, "not_found", "agent not found")
		return
	}
	descriptor, _ := runtimes.Describe(agent.Spec.Runtime)
	mode, _ := descriptor.Auth(agent.Spec.Secrets.AuthType)
	switch mode.Flow {
	case "device-code":
		s.handleCodexDeviceAuth(w, r, name)
	case "authorization-code":
		s.handleReauthorize(w, r, name)
	default:
		writeJSONError(w, 409, "invalid_auth_mode", "interactive authentication is not supported for this agent")
	}
}
