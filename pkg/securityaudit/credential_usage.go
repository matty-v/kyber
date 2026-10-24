package securityaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
)

const MaxCredentialUsageItems = 256

var credentialUsageActions = map[string]bool{
	"api:read": true, "api:write": true, "agents:read": true, "agents:write": true,
	"machines:read": true, "machines:write": true, "tasks:read": true, "tasks:write": true,
	"requests:read": true, "requests:write": true, "usage:read": true,
	"credentials:read": true, "credentials:write": true, "audit:read": true,
	"agents:exec": true, "machines:exec": true, "logs:read": true, "transcripts:read": true,
	"secrets:read": true, "secrets:write": true, "archives:read": true, "archives:write": true,
	"browser:sign-in": true,
}

func ValidCredentialUsageAction(action string) bool { return credentialUsageActions[action] }

// CredentialUsageIdentity is established by current authentication, never by
// inspecting or fingerprinting unrecognized credential material.
type CredentialUsageIdentity struct {
	CredentialID string `json:"credentialId"`
	PrincipalID  string `json:"principalId"`
	Authority    string `json:"authority"`
}

func (i CredentialUsageIdentity) valid() bool {
	return validActorID(i.CredentialID) && validActorID(i.PrincipalID) && (i.Authority == "configuration" || i.Authority == "registry")
}

type CredentialUsageDelta struct {
	CredentialUsageIdentity
	Transport      string            `json:"transport"`
	Authenticated  uint64            `json:"authenticated"`
	Denied         uint64            `json:"denied"`
	LastAcceptedAt *time.Time        `json:"lastAcceptedAt"`
	Generation     uint64            `json:"generation"`
	Actions        map[string]uint64 `json:"actions"`
}

func (d CredentialUsageDelta) key() string {
	return d.Authority + "\x00" + d.CredentialID + "\x00" + d.PrincipalID + "\x00" + d.Transport
}

type CredentialUsageBatch struct {
	Namespace    string                 `json:"namespace"`
	ObserverID   string                 `json:"observerId"`
	PodUID       string                 `json:"podUid"`
	StartedAt    time.Time              `json:"startedAt"`
	Sequence     int64                  `json:"sequence"`
	Stopped      bool                   `json:"stopped"`
	Dropped      uint64                 `json:"dropped"`
	Unrecognized uint64                 `json:"unrecognized"`
	Unattributed uint64                 `json:"unattributed"`
	Unavailable  uint64                 `json:"unavailable"`
	Items        []CredentialUsageDelta `json:"items"`
}

func validUsageTime(at time.Time) bool { return !at.IsZero() && at.Year() >= 2000 && at.Year() <= 9999 }
func (b CredentialUsageBatch) normalized() CredentialUsageBatch {
	b.StartedAt = b.StartedAt.UTC().Truncate(time.Microsecond)
	b.Items = append([]CredentialUsageDelta{}, b.Items...)
	for i := range b.Items {
		item := &b.Items[i]
		if item.LastAcceptedAt != nil {
			at := item.LastAcceptedAt.UTC().Truncate(time.Microsecond)
			item.LastAcceptedAt = &at
		}
		if item.Actions == nil {
			item.Actions = map[string]uint64{}
		}
	}
	sort.Slice(b.Items, func(i, j int) bool { return b.Items[i].key() < b.Items[j].key() })
	return b
}
func (b CredentialUsageBatch) Validate() error {
	id, err := uuid.Parse(b.ObserverID)
	if err != nil || id == uuid.Nil || id.String() != b.ObserverID || !namespaceName.MatchString(b.Namespace) || !validUsageTime(b.StartedAt) || b.Sequence < 1 || len(b.Items) > MaxCredentialUsageItems {
		return ErrInvalid
	}
	if b.PodUID != "" {
		if id, err := uuid.Parse(b.PodUID); err != nil || id == uuid.Nil || id.String() != b.PodUID {
			return ErrInvalid
		}
	}
	seen := map[string]bool{}
	for _, item := range b.Items {
		if !item.CredentialUsageIdentity.valid() || seen[item.key()] || len(item.Actions) > len(credentialUsageActions) {
			return ErrInvalid
		}
		seen[item.key()] = true
		switch item.Transport {
		case "bearer", "browser-session", "archive-capability":
		default:
			return ErrInvalid
		}
		if item.Authenticated == 0 {
			if item.Denied == 0 || item.LastAcceptedAt != nil || item.Generation != 0 {
				return ErrInvalid
			}
		} else if item.LastAcceptedAt == nil || !validUsageTime(*item.LastAcceptedAt) || item.Generation == 0 {
			return ErrInvalid
		}
		var count uint64
		for action, n := range item.Actions {
			if !credentialUsageActions[action] || n == 0 || math.MaxUint64-count < n {
				return ErrInvalid
			}
			count += n
		}
		if count != item.Authenticated {
			return ErrInvalid
		}
	}
	return nil
}

type CredentialUsageWriter interface {
	WriteCredentialUsage(context.Context, CredentialUsageBatch) error
}

// WriteCredentialUsage commits aggregate deltas and their replay cursor in one
// transaction, independently of the required-audit commit lock. Only the latest
// bounded payload is retained; old cursors must not be deleted and recreated.
func (s *PostgresStore) WriteCredentialUsage(ctx context.Context, batch CredentialUsageBatch) error {
	batch = batch.normalized()
	if batch.Validate() != nil {
		return ErrInvalid
	}
	payload, err := json.Marshal(batch)
	if err != nil || len(payload) > 1024*1024 {
		return ErrInvalid
	}
	items, err := json.Marshal(batch.Items)
	if err != nil {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(ctx, err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO operator_credential_usage_observers(namespace,observer_id,started_at,pod_uid,sequence) VALUES($1,$2,$3,$4,0) ON CONFLICT DO NOTHING`, batch.Namespace, batch.ObserverID, batch.StartedAt, batch.PodUID)
	if err != nil {
		return databaseError(ctx, err)
	}
	var sequence int64
	var same, metadataSame, stopped bool
	err = tx.QueryRowContext(ctx, `SELECT sequence,COALESCE(payload=$3::jsonb,false),started_at=$4 AND pod_uid=$5,stopped FROM operator_credential_usage_observers WHERE namespace=$1 AND observer_id=$2 FOR UPDATE`, batch.Namespace, batch.ObserverID, payload, batch.StartedAt, batch.PodUID).Scan(&sequence, &same, &metadataSame, &stopped)
	if err != nil {
		return databaseError(ctx, err)
	}
	if !metadataSame {
		return ErrConflict
	}
	if sequence == batch.Sequence {
		if !same {
			return ErrConflict
		}
		// An acknowledgement retry is not new collection progress/heartbeat.
		return databaseError(ctx, tx.Commit())
	}
	if stopped || sequence == math.MaxInt64 || sequence+1 != batch.Sequence {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO operator_credential_usage_totals AS existing(namespace,credential_id,principal_id,authority,transport,authenticated,denied,last_accepted_at,generation,actions)
 SELECT $1,i."credentialId",i."principalId",i.authority,i.transport,i.authenticated,i.denied,i."lastAcceptedAt",i.generation,i.actions
 FROM jsonb_to_recordset($2::jsonb) AS i("credentialId" text,"principalId" text,authority text,transport text,authenticated numeric,denied numeric,"lastAcceptedAt" timestamptz,generation numeric,actions jsonb) WHERE true
 ON CONFLICT(namespace,credential_id,principal_id,authority,transport) DO UPDATE SET
 authenticated=existing.authenticated+excluded.authenticated,denied=existing.denied+excluded.denied,
 last_accepted_at=GREATEST(existing.last_accepted_at,excluded.last_accepted_at),
 generation=CASE WHEN excluded.last_accepted_at IS NOT NULL AND (existing.last_accepted_at IS NULL OR excluded.last_accepted_at>existing.last_accepted_at) THEN excluded.generation WHEN excluded.last_accepted_at=existing.last_accepted_at THEN GREATEST(existing.generation,excluded.generation) ELSE existing.generation END,
 actions=(SELECT COALESCE(jsonb_object_agg(key,total),'{}'::jsonb) FROM
 (SELECT key,SUM(value::numeric) total FROM
 (SELECT key,value FROM jsonb_each_text(existing.actions) UNION ALL SELECT key,value FROM jsonb_each_text(excluded.actions)) counts GROUP BY key) merged)`, batch.Namespace, items)
	if err != nil {
		return databaseError(ctx, err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE operator_credential_usage_observers SET sequence=$3,payload=$4::jsonb,progress_at=clock_timestamp(),stopped=$5,dropped=dropped+$6,unrecognized=unrecognized+$7,unavailable=unavailable+$8,unattributed=unattributed+$9 WHERE namespace=$1 AND observer_id=$2`, batch.Namespace, batch.ObserverID, batch.Sequence, payload, batch.Stopped, batch.Dropped, batch.Unrecognized, batch.Unavailable, batch.Unattributed)
	if err != nil {
		return databaseError(ctx, err)
	}
	return databaseError(ctx, tx.Commit())
}

type CredentialUsageTotals struct {
	CredentialUsageIdentity
	Transport      string            `json:"transport"`
	Authenticated  string            `json:"authenticated"`
	Denied         string            `json:"denied"`
	LastAcceptedAt *time.Time        `json:"lastAcceptedAt,omitempty"`
	Generation     string            `json:"generation"`
	Actions        map[string]string `json:"actions"`
}
type CredentialUsageCoverage struct {
	Since                   *time.Time `json:"since,omitempty"`
	LatestProgress          *time.Time `json:"latestProgress,omitempty"`
	CurrentObserverReported bool       `json:"currentObserverReported"`
	CurrentObserverFresh    bool       `json:"currentObserverFresh"`
	FreshObservers          int        `json:"freshObservers"`
	StaleObservers          int        `json:"staleObservers"`
	Dropped                 string     `json:"dropped"`
	Unrecognized            string     `json:"unrecognized"`
	Unattributed            string     `json:"unattributed"`
	Unavailable             string     `json:"unavailable"`
}
type CredentialUsageView struct {
	Coverage CredentialUsageCoverage `json:"coverage"`
	Items    []CredentialUsageTotals `json:"items"`
}
type CredentialUsageReader interface {
	ReadCredentialUsage(context.Context, string, string, []CredentialUsageIdentity) (CredentialUsageView, error)
}

// ReadCredentialUsage joins only exact current identities. Reusing an opaque
// credential ID for another principal cannot merge the old principal's activity.
func (s *PostgresStore) ReadCredentialUsage(ctx context.Context, namespace, observerID string, identities []CredentialUsageIdentity) (CredentialUsageView, error) {
	result := CredentialUsageView{Items: []CredentialUsageTotals{}}
	if !namespaceName.MatchString(namespace) || len(identities) > MaxCredentialUsageItems {
		return result, ErrInvalid
	}
	if id, err := uuid.Parse(observerID); err != nil || id == uuid.Nil || id.String() != observerID {
		return result, ErrInvalid
	}
	seen := map[string]bool{}
	for _, identity := range identities {
		key := identity.Authority + "\x00" + identity.CredentialID + "\x00" + identity.PrincipalID
		if seen[key] {
			return result, ErrInvalid
		}
		seen[key] = true
		if !identity.valid() {
			return result, ErrInvalid
		}
	}
	requested, err := json.Marshal(append([]CredentialUsageIdentity{}, identities...))
	if err != nil {
		return result, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return result, databaseError(ctx, err)
	}
	defer tx.Rollback()
	var since, latest sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT MIN(started_at),MAX(progress_at),COALESCE(bool_or(observer_id=$2),false),COALESCE(bool_or(observer_id=$2 AND NOT stopped AND progress_at>=clock_timestamp()-interval '15 seconds'),false),COUNT(*) FILTER(WHERE NOT stopped AND progress_at>=clock_timestamp()-interval '15 seconds'),COUNT(*) FILTER(WHERE NOT stopped AND progress_at<clock_timestamp()-interval '15 seconds'),COALESCE(SUM(dropped),0)::text,COALESCE(SUM(unrecognized),0)::text,COALESCE(SUM(unavailable),0)::text,COALESCE(SUM(unattributed),0)::text FROM operator_credential_usage_observers WHERE namespace=$1 AND sequence>0`, namespace, observerID).Scan(&since, &latest, &result.Coverage.CurrentObserverReported, &result.Coverage.CurrentObserverFresh, &result.Coverage.FreshObservers, &result.Coverage.StaleObservers, &result.Coverage.Dropped, &result.Coverage.Unrecognized, &result.Coverage.Unavailable, &result.Coverage.Unattributed)
	if err != nil {
		return result, databaseError(ctx, err)
	}
	if since.Valid {
		at := since.Time.UTC()
		result.Coverage.Since = &at
	}
	if latest.Valid {
		at := latest.Time.UTC()
		result.Coverage.LatestProgress = &at
	}
	rows, err := tx.QueryContext(ctx, `SELECT totals.credential_id,totals.principal_id,totals.authority,totals.transport,totals.authenticated::text,totals.denied::text,totals.last_accepted_at,totals.generation::text,totals.actions FROM operator_credential_usage_totals totals JOIN jsonb_to_recordset($2::jsonb) AS wanted("credentialId" text,"principalId" text,authority text) ON totals.credential_id=wanted."credentialId" AND totals.principal_id=wanted."principalId" AND totals.authority=wanted.authority WHERE totals.namespace=$1 ORDER BY totals.credential_id,totals.principal_id,totals.authority,totals.transport LIMIT 769`, namespace, requested)
	if err != nil {
		return result, databaseError(ctx, err)
	}
	for rows.Next() {
		var item CredentialUsageTotals
		var accepted sql.NullTime
		var actions []byte
		if rows.Scan(&item.CredentialID, &item.PrincipalID, &item.Authority, &item.Transport, &item.Authenticated, &item.Denied, &accepted, &item.Generation, &actions) != nil {
			rows.Close()
			return result, ErrUnavailable
		}
		var values map[string]json.Number
		if json.Unmarshal(actions, &values) != nil {
			rows.Close()
			return result, ErrUnavailable
		}
		item.Actions = map[string]string{}
		for action, n := range values {
			if !credentialUsageActions[action] {
				rows.Close()
				return result, ErrUnavailable
			}
			item.Actions[action] = n.String()
		}
		if accepted.Valid {
			at := accepted.Time.UTC()
			item.LastAcceptedAt = &at
		}
		result.Items = append(result.Items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return result, databaseError(ctx, err)
	}
	if err = rows.Close(); err != nil {
		return result, databaseError(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return result, databaseError(ctx, err)
	}
	return result, nil
}

var _ CredentialUsageWriter = (*PostgresStore)(nil)
var _ CredentialUsageReader = (*PostgresStore)(nil)
