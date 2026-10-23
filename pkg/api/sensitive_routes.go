package api

import (
	"net/http"
	"strings"
)

func requiredAgentLogScope(r *http.Request) Scope {
	switch r.URL.Query().Get("source") {
	case "", "kubelet", "archive":
		return ScopeLogsRead
	case "transcript":
		return ScopeTranscriptsRead
	default:
		return ""
	}
}

// Enumerate method/action pairs; future subpaths do not inherit grants.
func agentSensitiveScope(r *http.Request, action string) (Scope, bool) {
	if r.Method == http.MethodGet {
		switch action {
		case "exec":
			return ScopeAgentExec, true
		case "logs":
			scope := requiredAgentLogScope(r)
			return scope, scope != ""
		case "secrets", "secrets/":
			return ScopeSecretsList, true
		}
	}
	if !strings.HasPrefix(action, "secrets/") {
		return "", false
	}
	key := strings.TrimPrefix(action, "secrets/")
	if key == "" || strings.Contains(key, "/") {
		return "", false
	}
	switch r.Method {
	case http.MethodGet:
		return ScopeSecretsRead, true
	case http.MethodPut, http.MethodDelete:
		return ScopeSecretsWrite, true
	default:
		return "", false
	}
}

func machineSensitiveScope(r *http.Request, action string) (Scope, bool) {
	if r.Method != http.MethodGet {
		return "", false
	}
	switch action {
	case "exec":
		return ScopeMachineExec, true
	case "logs":
		if source := r.URL.Query().Get("source"); source != "" && source != "kubelet" {
			return "", false
		}
		return ScopeMachineLogsRead, true
	default:
		return "", false
	}
}
