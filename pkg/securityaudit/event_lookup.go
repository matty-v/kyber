package securityaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

var ErrEventNotFound = errors.New("security event not found")

// LookupEvent returns one durable, value-free authorization record for a native
// operation. It does not prove current access or authorize another effect.
// Pending operation intents remain the recovery source after history retention.
func (s *PostgresStore) LookupEvent(ctx context.Context, namespace, eventID string) (Record, error) {
	if s == nil || !namespaceName.MatchString(namespace) || !validMaintenanceUID(eventID) {
		return Record{}, ErrInvalid
	}
	var record Record
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT sequence,stored_at,document FROM operator_security_events WHERE namespace=$1 AND id=$2`, namespace, eventID).Scan(&record.Sequence, &record.StoredAt, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrEventNotFound
	}
	if err != nil {
		return Record{}, databaseError(ctx, err)
	}
	if json.Unmarshal(raw, &record.Event) != nil || record.Event.Validate() != nil || record.ID != eventID || record.Namespace != namespace || record.Sequence < 1 || record.StoredAt.IsZero() {
		return Record{}, ErrUnavailable
	}
	record.Event = record.Event.normalized()
	record.StoredAt = record.StoredAt.UTC()
	return record, nil
}
