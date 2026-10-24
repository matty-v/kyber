package securityaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const commitLock = `SELECT pg_advisory_xact_lock(719046214305181)`

// PostgresStore does not cache events or turn database failures into empty
// history. A new instance against the same database sees the same records.
type PostgresStore struct{ db *sql.DB }

func NewPostgresStore(db *sql.DB) (*PostgresStore, error) {
	if db == nil {
		return nil, ErrUnavailable
	}
	return &PostgresStore{db: db}, nil
}
func (s *PostgresStore) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS operator_security_events (
 id UUID PRIMARY KEY, sequence BIGSERIAL UNIQUE NOT NULL,
 operation_id UUID NOT NULL, namespace TEXT NOT NULL,
 observed_at TIMESTAMPTZ NOT NULL, stored_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 principal_id TEXT NOT NULL, credential_id TEXT NOT NULL, action TEXT NOT NULL,
 resource_kind TEXT NOT NULL, resource_name TEXT NOT NULL, document JSONB NOT NULL
 );
 CREATE INDEX IF NOT EXISTS operator_security_events_namespace_sequence ON operator_security_events(namespace,sequence DESC);
 CREATE INDEX IF NOT EXISTS operator_security_events_retention ON operator_security_events(stored_at,sequence);
 CREATE INDEX IF NOT EXISTS operator_security_events_credential ON operator_security_events(namespace,credential_id,sequence DESC);
 CREATE TABLE IF NOT EXISTS operator_credential_operations (
 namespace TEXT NOT NULL, operation_id UUID NOT NULL,
 admitted_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 intent JSONB NOT NULL, completion JSONB,
 PRIMARY KEY(namespace,operation_id)
 );
 CREATE INDEX IF NOT EXISTS operator_credential_operations_pending ON operator_credential_operations(namespace,operation_id) WHERE completion IS NULL;
 CREATE TABLE IF NOT EXISTS operator_credential_usage_observers (
 namespace TEXT NOT NULL,observer_id UUID NOT NULL,started_at TIMESTAMPTZ NOT NULL,pod_uid TEXT NOT NULL,
 sequence BIGINT NOT NULL, payload JSONB,progress_at TIMESTAMPTZ,stopped BOOLEAN NOT NULL DEFAULT false,
 dropped NUMERIC(40,0) NOT NULL DEFAULT 0 CHECK(dropped>=0),unrecognized NUMERIC(40,0) NOT NULL DEFAULT 0 CHECK(unrecognized>=0),unavailable NUMERIC(40,0) NOT NULL DEFAULT 0 CHECK(unavailable>=0),unattributed NUMERIC(40,0) NOT NULL DEFAULT 0 CHECK(unattributed>=0),
 PRIMARY KEY(namespace,observer_id)
 );
 CREATE TABLE IF NOT EXISTS operator_credential_usage_totals (
 namespace TEXT NOT NULL,credential_id TEXT NOT NULL,principal_id TEXT NOT NULL,authority TEXT NOT NULL CHECK(authority IN ('configuration','registry')),transport TEXT NOT NULL CHECK(transport IN ('bearer','browser-session','archive-capability')),
 authenticated NUMERIC(40,0) NOT NULL CHECK(authenticated>=0),denied NUMERIC(40,0) NOT NULL CHECK(denied>=0),last_accepted_at TIMESTAMPTZ,generation NUMERIC(20,0) NOT NULL CHECK(generation>=0),actions JSONB NOT NULL,
 PRIMARY KEY(namespace,credential_id,principal_id,authority,transport)
 );`)
	return databaseError(ctx, err)
}

// Append is idempotent only for identical content. The lock orders commits
// before sequences are assigned, allowing a query to pin a reliable high-water
// mark even while other replicas append backdated observations.
func (s *PostgresStore) Append(ctx context.Context, e Event) (Record, error) {
	e = e.normalized()
	if err := e.Validate(); err != nil {
		return Record{}, err
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return Record{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, databaseError(ctx, err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, commitLock); err != nil {
		return Record{}, databaseError(ctx, err)
	}
	got, err := appendEventInTransaction(ctx, tx, e, payload)
	if err != nil {
		return Record{}, err
	}
	if err = tx.Commit(); err != nil {
		return Record{}, databaseError(ctx, err)
	}
	got.Event = e
	got.StoredAt = got.StoredAt.UTC()
	return got, nil
}

// The caller holds commitLock until commit, preserving history ordering across
// ordinary appends and atomic credential admission.
func appendEventInTransaction(ctx context.Context, tx *sql.Tx, e Event, payload []byte) (Record, error) {
	_, err := tx.ExecContext(ctx, `INSERT INTO operator_security_events(id,operation_id,namespace,observed_at,principal_id,credential_id,action,resource_kind,resource_name,document)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(id) DO NOTHING`, e.ID, e.OperationID, e.Namespace, e.ObservedAt, e.PrincipalID, e.CredentialID, e.Action, e.ResourceKind, e.ResourceName, payload)
	if err != nil {
		return Record{}, databaseError(ctx, err)
	}
	var got Record
	var same bool
	err = tx.QueryRowContext(ctx, `SELECT sequence,stored_at,document=$2::jsonb FROM operator_security_events WHERE id=$1`, e.ID, payload).Scan(&got.Sequence, &got.StoredAt, &same)
	if err != nil {
		return Record{}, databaseError(ctx, err)
	}
	if !same {
		return Record{}, ErrConflict
	}
	got.Event = e
	got.StoredAt = got.StoredAt.UTC()
	return got, nil
}

// Filter is a whitelist. Namespace is mandatory even for administrator queries.
// Until is exclusive. Cursor belongs to the same Filter and must be bound to it
// by any future public API; it is not an externally trusted authorization token.
type Filter struct {
	Namespace                                                     string
	Since, Until                                                  time.Time
	PrincipalID, CredentialID, Action, ResourceKind, ResourceName string
	Limit                                                         int
	// Omit false when binding cursors, preserving existing default continuations.
	ExcludeRoutineAuditReads bool `json:",omitempty"`
}
type Cursor struct{ MaxSequence, BeforeSequence int64 }
type Page struct {
	Items    []Record
	Next     *Cursor
	Snapshot int64
}

// Validate checks the same bounded whitelist used by Query without touching
// storage, so public readers can reject invalid input before audit admission.
func (f Filter) Validate() error {
	if !f.valid() {
		return ErrInvalid
	}
	return nil
}

func (f Filter) valid() bool {
	if !namespaceName.MatchString(f.Namespace) || f.Limit < 1 || f.Limit > 200 {
		return false
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Since.Before(f.Until) {
		return false
	}
	for _, v := range []string{f.PrincipalID, f.CredentialID} {
		if v != "" && !validActorID(v) {
			return false
		}
	}
	if f.Action != "" && !actions[f.Action] {
		return false
	}
	switch f.ResourceKind {
	case "":
		return f.ResourceName == ""
	case "installation":
		return f.ResourceName == ""
	case "agent", "machine":
		return f.ResourceName == "" || resourceName.MatchString(f.ResourceName)
	case "log-container":
		return f.ResourceName == "" || validActorID(f.ResourceName)
	case "credential":
		return f.ResourceName == "" || validActorID(f.ResourceName)
	default:
		return false
	}
}
func (s *PostgresStore) Query(ctx context.Context, f Filter, cursor *Cursor) (Page, error) {
	page := Page{Items: []Record{}}
	if !f.valid() {
		return page, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, databaseError(ctx, err)
	}
	defer tx.Rollback()
	if cursor == nil {
		if _, err = tx.ExecContext(ctx, commitLock); err != nil {
			return page, databaseError(ctx, err)
		}
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM operator_security_events WHERE namespace=$1`, f.Namespace).Scan(&page.Snapshot)
		if err != nil {
			return page, databaseError(ctx, err)
		}
	} else {
		if cursor.MaxSequence < 1 || cursor.BeforeSequence < 1 || cursor.BeforeSequence > cursor.MaxSequence {
			return page, ErrInvalid
		}
		page.Snapshot = cursor.MaxSequence
	}
	var before any
	if cursor != nil {
		before = cursor.BeforeSequence
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,stored_at,document FROM operator_security_events WHERE namespace=$1 AND sequence<=$2 AND ($3::bigint IS NULL OR sequence<$3)
 AND ($4::timestamptz IS NULL OR observed_at >= $4) AND ($5::timestamptz IS NULL OR observed_at < $5)
 AND ($6='' OR principal_id=$6) AND ($7='' OR credential_id=$7) AND ($8='' OR action=$8)
 AND ($9='' OR resource_kind=$9) AND ($10='' OR resource_name=$10)
 AND ($12::boolean=false OR action<>'audit:read' OR COALESCE(document->>'outcome','unknown') NOT IN ('allow','success')) ORDER BY sequence DESC LIMIT $11`, f.Namespace, page.Snapshot, before, optionalTime(f.Since), optionalTime(f.Until), f.PrincipalID, f.CredentialID, f.Action, f.ResourceKind, f.ResourceName, f.Limit+1, f.ExcludeRoutineAuditReads)
	if err != nil {
		return page, databaseError(ctx, err)
	}
	for rows.Next() {
		var record Record
		var doc []byte
		if err = rows.Scan(&record.Sequence, &record.StoredAt, &doc); err != nil {
			rows.Close()
			return Page{}, databaseError(ctx, err)
		}
		if err = json.Unmarshal(doc, &record.Event); err != nil {
			rows.Close()
			return Page{}, ErrUnavailable
		}
		record.StoredAt = record.StoredAt.UTC()
		page.Items = append(page.Items, record)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Page{}, databaseError(ctx, err)
	}
	if len(page.Items) > f.Limit {
		page.Items = page.Items[:f.Limit]
		page.Next = &Cursor{MaxSequence: page.Snapshot, BeforeSequence: page.Items[len(page.Items)-1].Sequence}
	}
	if err = tx.Commit(); err != nil {
		return Page{}, databaseError(ctx, err)
	}
	return page, nil
}

// Prune removes at most batch rows recorded before cutoff in one namespace.
// Use recorded time, not supplied observation time, to avoid prematurely
// expiring late/backdated observations. Retention configuration lives above
// this storage primitive and must be validated before calling it.
func (s *PostgresStore) Prune(ctx context.Context, namespace string, cutoff time.Time, batch int) (int64, error) {
	return s.EnforceRetention(ctx, namespace, cutoff, 0, batch)
}

// EnforceRetention also bounds retained row count. The cap is enforced by the
// periodic bounded worker, so a burst can temporarily exceed it between sweeps.
// Zero keep means age-only pruning for the lower-level Prune interface.
func (s *PostgresStore) EnforceRetention(ctx context.Context, namespace string, cutoff time.Time, keep int64, batch int) (int64, error) {
	if !namespaceName.MatchString(namespace) || cutoff.IsZero() || keep < 0 || batch < 1 || batch > 1000 {
		return 0, ErrInvalid
	}
	r, err := s.db.ExecContext(ctx, `DELETE FROM operator_security_events WHERE id IN (
 SELECT id FROM operator_security_events WHERE namespace=$1 AND (stored_at<$2 OR ($3::bigint>0 AND sequence<=COALESCE((SELECT sequence FROM operator_security_events WHERE namespace=$1 ORDER BY sequence DESC OFFSET $3 LIMIT 1),0)))
 ORDER BY sequence LIMIT $4)`, namespace, cutoff.UTC(), keep, batch)
	if err != nil {
		return 0, databaseError(ctx, err)
	}
	n, err := r.RowsAffected()
	return n, databaseError(ctx, err)
}

func optionalTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
func storeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrUnavailable
}

// Some PostgreSQL drivers return a statement-cancellation error instead of
// wrapping the context deadline. Preserve that distinction without exposing
// the driver's response text.
func databaseError(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return storeError(err)
}
