package taskstore

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type listCursor struct {
	Version   int    `json:"v"`
	CreatedAt string `json:"createdAt"`
	ID        string `json:"id"`
	Filter    string `json:"filter"`
	Checksum  string `json:"checksum"`
}

func filterHash(p ListParams) string {
	states := make([]string, 0, len(p.States))
	for _, state := range p.States {
		states = append(states, string(state))
	}
	s := sha256.Sum256([]byte(p.Agent.Namespace + "\x00" + p.Agent.Name + "\x00" + string(p.State) + "\x00" + strings.Join(states, ",") + "\x00" + p.Correlation + "\x00" + p.UpdatedAfter.UTC().Format(time.RFC3339Nano) + "\x00" + p.Authorization.TenantID + "\x00" + p.Authorization.PrincipalID + "\x00" + p.Authorization.AgentResourceID))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

func encodeCursor(p ListParams, t *Task) (string, error) {
	c := listCursor{Version: 2, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339Nano), ID: t.ID, Filter: filterHash(p)}
	s := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s", c.Version, c.CreatedAt, c.ID, c.Filter)))
	c.Checksum = base64.RawURLEncoding.EncodeToString(s[:])
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeCursor(value string, p ListParams) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return time.Time{}, "", ErrInvalidCursor
	}
	var c listCursor
	if json.Unmarshal(b, &c) != nil || c.Version != 2 || c.ID == "" || c.Filter != filterHash(p) {
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
