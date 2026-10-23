package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/matty-v/kyber/internal/testutil"
	"github.com/matty-v/kyber/pkg/api"
	"github.com/matty-v/kyber/pkg/securityaudit"
)

type historyStore struct {
	*testutil.SecurityAudit
	queries int
	filter  securityaudit.Filter
	cursor  *securityaudit.Cursor
	page    securityaudit.Page
	err     error
}

func (s *historyStore) Query(ctx context.Context, f securityaudit.Filter, c *securityaudit.Cursor) (securityaudit.Page, error) {
	if _, ok := ctx.Deadline(); !ok {
		panic("unbounded history query")
	}
	s.queries++
	s.filter = f
	s.cursor = c
	return s.page, s.err
}

func TestSecurityHistoryBoundedFilterPaginationAndAudit(t *testing.T) {
	store := &historyStore{SecurityAudit: testutil.NewSecurityAudit(), page: securityaudit.Page{Snapshot: 20, Next: &securityaudit.Cursor{MaxSequence: 20, BeforeSequence: 15}}}
	s := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SecurityAudit: store}
	h := s.BuildHandler()
	query := "?limit=2&credentialId=issuer%2Fmonitor&action=logs%3Aread&since=2026-10-23T00%3A00%3A00Z"
	get := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/v1/security-events"+query, nil)
		r.Header.Set("Authorization", "Bearer "+testAPIKey)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := get(query)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status/cache %d %s", w.Code, w.Header().Get("Cache-Control"))
	}
	var body struct {
		NextCursor string                 `json:"nextCursor"`
		Coverage   string                 `json:"coverage"`
		Items      []securityaudit.Record `json:"items"`
		Warnings   []string               `json:"warnings"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.NextCursor == "" || body.Items == nil || body.Coverage != "partial-sensitive-access" || len(body.Warnings) == 0 {
		t.Fatal("missing pagination or honest coverage")
	}
	if store.filter.Namespace != "kyber-system" || store.filter.Limit != 2 || store.filter.CredentialID != "issuer/monitor" || store.filter.Action != "logs:read" {
		t.Fatal("filters not passed exactly")
	}
	if records := store.Records(); len(records) != 2 || records[0].Action != "audit:read" || records[0].Outcome != "allow" || records[1].Phase != "completion" {
		t.Fatal("history read was not audited")
	}
	w = get(query + "&cursor=" + url.QueryEscape(body.NextCursor))
	if w.Code != 200 || store.cursor == nil || store.cursor.MaxSequence != 20 || store.cursor.BeforeSequence != 15 {
		t.Fatal("continuation changed snapshot")
	}
	before := store.queries
	w = get("?limit=3&cursor=" + url.QueryEscape(body.NextCursor))
	if w.Code != 400 || store.queries != before {
		t.Fatal("cursor reused for another filter")
	}
	w = get(query + "&includeHistoryViews=false&cursor=" + url.QueryEscape(body.NextCursor))
	if w.Code != 400 || store.queries != before {
		t.Fatal("cursor reused across history-view selection")
	}
	w = get(query + "&includeHistoryViews=false")
	if w.Code != 200 || !store.filter.ExcludeRoutineAuditReads {
		t.Fatal("history-view filter was not applied")
	}
}

func TestSecurityHistoryInvalidInputDoesNotReadOrEcho(t *testing.T) {
	store := &historyStore{SecurityAudit: testutil.NewSecurityAudit()}
	h := (&api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SecurityAudit: store}).BuildHandler()
	for _, query := range []string{"?limit=201", "?limit=0", "?limit=1&limit=2", "?namespace=other", "?action=private-marker", "?since=wrong", "?since=2026-10-24T00:00:00Z&until=2026-10-23T00:00:00Z", "?cursor=private-marker", "?credentialId=", "?since=%xx", "?resourceName=alpha", "?resourceKind=unknown", "?includeHistoryViews=0"} {
		r := httptest.NewRequest("GET", "/api/v1/security-events"+query, nil)
		r.Header.Set("Authorization", "Bearer "+testAPIKey)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || strings.Contains(w.Body.String(), "private-marker") {
			t.Fatalf("invalid query status or echo: %d", w.Code)
		}
	}
	if store.queries != 0 || len(store.Records()) != 0 {
		t.Fatal("invalid input reached store")
	}
}

func TestSecurityHistoryFailsClosedBeforeQueryAndRedactsStoreErrors(t *testing.T) {
	for _, mode := range []string{"missing", "admission", "query"} {
		t.Run(mode, func(t *testing.T) {
			store := &historyStore{SecurityAudit: testutil.NewSecurityAudit()}
			s := &api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SecurityAudit: store}
			if mode == "missing" {
				s.SecurityAudit = nil
			}
			if mode == "admission" {
				store.Fail = errors.New("private-marker")
			}
			if mode == "query" {
				store.err = errors.New("private-marker")
			}
			r := httptest.NewRequest("GET", "/api/v1/security-events", nil)
			r.Header.Set("Authorization", "Bearer "+testAPIKey)
			w := httptest.NewRecorder()
			s.BuildHandler().ServeHTTP(w, r)
			if w.Code != 503 || strings.Contains(w.Body.String(), "private-marker") || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("store outage appeared as history or leaked detail")
			}
			if mode != "query" && store.queries != 0 {
				t.Fatal("query before required admission")
			}
		})
	}
}

func TestSecurityHistoryRestrictedBearerAndSessionCannotQuery(t *testing.T) {
	for _, browser := range []bool{false, true} {
		store := &historyStore{SecurityAudit: testutil.NewSecurityAudit()}
		h := (&api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SecurityAudit: store, Callers: []api.ScopedCaller{{Name: "history-reader", Key: "restricted-history-test", Scopes: []string{"metrics:read"}}}}).BuildHandler()
		r := httptest.NewRequest("GET", "/api/v1/security-events", nil)
		if browser {
			login := httptest.NewRequest(http.MethodPost, "/api/v1/browser-session", nil)
			login.Header.Set("Authorization", "Bearer restricted-history-test")
			res := httptest.NewRecorder()
			h.ServeHTTP(res, login)
			if res.Code != http.StatusNoContent {
				t.Fatalf("session exchange %d", res.Code)
			}
			for _, cookie := range res.Result().Cookies() {
				r.AddCookie(cookie)
			}
		} else {
			r.Header.Set("Authorization", "Bearer restricted-history-test")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 || store.queries != 0 {
			t.Fatal("restricted history read")
		}
	}
}

func TestSecurityHistoryWideNumbersAreLosslessStrings(t *testing.T) {
	store := &historyStore{SecurityAudit: testutil.NewSecurityAudit(), page: securityaudit.Page{
		Snapshot: 9007199254740993, Items: []securityaudit.Record{{Sequence: 9007199254740993, Event: securityaudit.Event{Generation: ^uint64(0)}}},
	}}
	h := (&api.Server{APIKey: testAPIKey, Namespace: "kyber-system", SecurityAudit: store}).BuildHandler()
	r := httptest.NewRequest("GET", "/api/v1/security-events", nil)
	r.Header.Set("Authorization", "Bearer "+testAPIKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body struct {
		Snapshot string `json:"snapshot"`
		Items    []struct {
			Generation string `json:"credentialGeneration"`
			Sequence   string `json:"sequence"`
		} `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Snapshot != "9007199254740993" || len(body.Items) != 1 || body.Items[0].Generation != "18446744073709551615" || body.Items[0].Sequence != "9007199254740993" {
		t.Fatal("wide identity counters lost precision")
	}
}
