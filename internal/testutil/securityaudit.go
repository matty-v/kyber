// Package testutil contains test fixtures, never runtime persistence fallbacks.
package testutil

import (
	"context"
	"reflect"
	"sync"
	"time"

	"github.com/matty-v/kyber/pkg/securityaudit"
)

// SecurityAudit is an in-memory test double. It makes required writes explicit
// in API fixtures without pretending that an installation has durable history.
type SecurityAudit struct {
	mu      sync.Mutex
	records []securityaudit.Record
	Fail    error
}

func NewSecurityAudit() *SecurityAudit { return &SecurityAudit{} }
func (s *SecurityAudit) Append(ctx context.Context, e securityaudit.Event) (securityaudit.Record, error) {
	if err := ctx.Err(); err != nil {
		return securityaudit.Record{}, err
	}
	if err := e.Validate(); err != nil {
		return securityaudit.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail != nil {
		return securityaudit.Record{}, s.Fail
	}
	for _, r := range s.records {
		if r.ID == e.ID {
			if !reflect.DeepEqual(r.Event, e) {
				return securityaudit.Record{}, securityaudit.ErrConflict
			}
			return r, nil
		}
	}
	r := securityaudit.Record{Event: e, Sequence: int64(len(s.records) + 1), StoredAt: time.Now().UTC()}
	s.records = append(s.records, r)
	return r, nil
}
func (s *SecurityAudit) Records() []securityaudit.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]securityaudit.Record(nil), s.records...)
}
func (s *SecurityAudit) Query(context.Context, securityaudit.Filter, *securityaudit.Cursor) (securityaudit.Page, error) {
	return securityaudit.Page{}, securityaudit.ErrUnavailable
}
func (s *SecurityAudit) Prune(context.Context, string, time.Time, int) (int64, error) {
	return 0, securityaudit.ErrUnavailable
}
