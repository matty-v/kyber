package usagestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// RouteRevision is trusted Agent configuration, supplied by the control
// plane, not by an in-pod reporter. EndpointURL participates only in an
// internal fingerprint; the table stores no URL and exposes a random UUID.
type RouteRevision struct {
	Runtime     string
	Kind        string
	EndpointURL string
}

func (r RouteRevision) Validate() error {
	switch r.Runtime {
	case "claude", "codex", "hermes":
	default:
		return ErrInvalid
	}
	switch r.Kind {
	case "native", "direct-api":
		if r.EndpointURL != "" {
			return ErrInvalid
		}
	case "openrouter", "custom":
		if r.EndpointURL == "" || len(r.EndpointURL) > 2048 {
			return ErrInvalid
		}
		u, err := url.Parse(r.EndpointURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return ErrInvalid
		}
		if r.Kind == "openrouter" && !(u.Scheme == "https" && strings.EqualFold(u.Hostname(), "openrouter.ai") &&
			(u.Port() == "" || u.Port() == "443") && strings.TrimSuffix(u.Path, "/") == "/api/v1") {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// ResolveRoute returns a random, stable ID for one Agent configuration
// revision. A changed endpoint or route kind gets a new ID. A credential
// rotation does not, because the credential never enters the revision.
func (s *PostgresStore) ResolveRoute(ctx context.Context, agentUID string, route RouteRevision) (string, error) {
	if agentUID == "" || len(agentUID) > 128 {
		return "", ErrInvalid
	}
	if err := route.Validate(); err != nil {
		return "", err
	}
	fingerprint := sha256.Sum256([]byte(route.Runtime + "\x00" + route.Kind + "\x00" + route.EndpointURL))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("starting usage route transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(9101172026)`); err != nil {
		return "", fmt.Errorf("locking usage routes: %w", err)
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT route_id FROM agent_usage_routes WHERE agent_uid=$1 AND fingerprint=$2`, agentUID, fingerprint[:]).Scan(&id)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("committing usage route lookup: %w", err)
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("looking up usage route: %w", err)
	}
	id = uuid.NewString()
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_usage_routes(agent_uid,fingerprint,route_id,configured_route) VALUES($1,$2,$3,$4)`, agentUID, fingerprint[:], id, route.Kind)
	if err != nil {
		return "", fmt.Errorf("recording usage route: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("committing usage route: %w", err)
	}
	return id, nil
}
