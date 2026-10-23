package securityaudit

import (
	"context"
	"time"
)

// Store is the persistence seam for authorization and history readers. API
// integration owns ordering, caller authorization and operation reconciliation.
// None of these methods treats a storage failure as success.
type Store interface {
	Append(context.Context, Event) (Record, error)
	Query(context.Context, Filter, *Cursor) (Page, error)
	Prune(context.Context, string, time.Time, int) (int64, error)
}

var _ Store = (*PostgresStore)(nil)
