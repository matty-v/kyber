//go:build integration

package contract

import (
	"context"
	"net/http"
	"testing"

	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

type historyContractStore struct{ *testutil.SecurityAudit }

func (s *historyContractStore) Query(_ context.Context, f securityaudit.Filter, _ *securityaudit.Cursor) (securityaudit.Page, error) {
	items := []securityaudit.Record{}
	for _, r := range s.Records() {
		if f.ExcludeRoutineAuditReads && r.Action == "audit:read" && (r.Outcome == "allow" || r.Outcome == "success") {
			continue
		}
		items = append(items, r)
	}
	return securityaudit.Page{Items: items, Snapshot: int64(len(s.Records()))}, nil
}

func TestContract_SecurityHistory(t *testing.T) {
	for _, tc := range []struct {
		query               string
		restricted, missing bool
		want                int
	}{
		{"", false, false, 200}, {"?limit=201", false, false, 400}, {"", true, false, 403}, {"", false, true, 503},
		{"?includeHistoryViews=false", false, false, 200},
	} {
		s := &api.Server{APIKey: contractAPIKey, Namespace: contractNS, SecurityAudit: &historyContractStore{testutil.NewSecurityAudit()}, Callers: []api.ScopedCaller{{Name: "restricted", Key: "history-contract-reader"}}}
		if tc.missing {
			s.SecurityAudit = nil
		}
		r := authedReq(t, http.MethodGet, "/api/v1/security-events"+tc.query, nil)
		if tc.restricted {
			r.Header.Set("Authorization", "Bearer history-contract-reader")
		}
		validate := validateContract
		if tc.want == 400 {
			validate = validateContractResponseOnly
		}
		w := validate(t, s.BuildHandler(), r)
		if w.Code != tc.want {
			t.Fatalf("status %d want %d", w.Code, tc.want)
		}
	}
}
