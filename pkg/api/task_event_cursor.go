package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/matty-v/kyber/pkg/taskstore"
)

const taskEventCursorVersion = 1

type taskEventCursorClaims struct {
	Version              int    `json:"v"`
	TaskID               string `json:"task"`
	TenantID             string `json:"tenant"`
	PrincipalID          string `json:"principal"`
	AgentResourceID      string `json:"agent"`
	CredentialID         string `json:"credential"`
	CredentialGeneration uint64 `json:"generation"`
	Sequence             int64  `json:"sequence"`
}

func (s *Server) taskEventCursorKey(r *http.Request) ([]byte, error) {
	caller := callerFrom(r.Context())
	if caller == nil {
		return nil, taskstore.ErrInvalidCursor
	}
	key := ""
	if s.auth != nil {
		if caller.Name == "legacy" {
			key = s.auth.currentKey()
		} else {
			key, _ = s.auth.scopedCallerKey(caller.Name)
		}
	} else {
		key = s.APIKey
	}
	if key == "" {
		return nil, taskstore.ErrInvalidCursor
	}
	return []byte(key), nil
}

func (s *Server) encodeTaskEventCursor(r *http.Request, agentName, taskID string, sequence int64) (string, error) {
	caller := callerFrom(r.Context())
	if caller == nil || sequence < 0 {
		return "", taskstore.ErrInvalidCursor
	}
	c := taskEventCursorClaims{Version: taskEventCursorVersion, TaskID: taskID, TenantID: caller.TenantID, PrincipalID: caller.PrincipalID, AgentResourceID: s.Namespace + "/" + agentName, CredentialID: caller.CredentialID, CredentialGeneration: caller.CredentialGeneration, Sequence: sequence}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(b)
	key, err := s.taskEventCursorKey(r)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("kyber-task-event-cursor-v1." + body))
	return strconv.Itoa(taskEventCursorVersion) + "." + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Server) decodeTaskEventCursor(r *http.Request, agentName, taskID, value string) (int64, error) {
	parts := splitCursor(value)
	if len(parts) != 3 || parts[0] != strconv.Itoa(taskEventCursorVersion) {
		return 0, taskstore.ErrInvalidCursor
	}
	presented, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, taskstore.ErrInvalidCursor
	}
	key, err := s.taskEventCursorKey(r)
	if err != nil {
		return 0, taskstore.ErrInvalidCursor
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("kyber-task-event-cursor-v1." + parts[1]))
	if !hmac.Equal(presented, mac.Sum(nil)) {
		return 0, taskstore.ErrInvalidCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, taskstore.ErrInvalidCursor
	}
	var c taskEventCursorClaims
	caller := callerFrom(r.Context())
	if json.Unmarshal(b, &c) != nil || caller == nil || c.Version != taskEventCursorVersion || c.TaskID != taskID || c.AgentResourceID != s.Namespace+"/"+agentName || c.TenantID != caller.TenantID || c.PrincipalID != caller.PrincipalID || c.CredentialID != caller.CredentialID || c.CredentialGeneration != caller.CredentialGeneration || c.Sequence < 0 {
		return 0, taskstore.ErrInvalidCursor
	}
	return c.Sequence, nil
}

func splitCursor(v string) []string {
	var out []string
	start := 0
	for i := 0; i < len(v); i++ {
		if v[i] == '.' {
			out = append(out, v[start:i])
			start = i + 1
		}
	}
	return append(out, v[start:])
}

var errTaskEventStreamUnavailable = errors.New("task event stream requires PostgreSQL")
