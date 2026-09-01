package taskstore

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

type listCursor struct {
	Version   int    `json:"v"`
	CreatedAt string `json:"createdAt"`
	ID        string `json:"id"`
	Filter    string `json:"filter"`
	Checksum  string `json:"checksum"`
}

func filterHash(agent AgentRef, state State, auth AuthorizationContext) string {
	s := sha256.Sum256([]byte(agent.Namespace + "\x00" + agent.Name + "\x00" + string(state) + "\x00" + auth.TenantID + "\x00" + auth.PrincipalID + "\x00" + auth.AgentResourceID))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

func encodeCursor(agent AgentRef, state State, auth AuthorizationContext, t *Task) (string, error) {
	c := listCursor{Version: 2, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339Nano), ID: t.ID, Filter: filterHash(agent, state, auth)}
	s := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s", c.Version, c.CreatedAt, c.ID, c.Filter)))
	c.Checksum = base64.RawURLEncoding.EncodeToString(s[:])
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeCursor(value string, agent AgentRef, state State, auth AuthorizationContext) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return time.Time{}, "", ErrInvalidCursor
	}
	var c listCursor
	if json.Unmarshal(b, &c) != nil || c.Version != 2 || c.ID == "" || c.Filter != filterHash(agent, state, auth) {
		return time.Time{}, "", ErrInvalidCursor
	}
	s := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s", c.Version, c.CreatedAt, c.ID, c.Filter)))
	if c.Checksum != base64.RawURLEncoding.EncodeToString(s[:]) {
		return time.Time{}, "", ErrInvalidCursor
	}
	t, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
	if err != nil {
		return time.Time{}, "", ErrInvalidCursor
	}
	return t, c.ID, nil
}
