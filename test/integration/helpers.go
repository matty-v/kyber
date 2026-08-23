//go:build integration

// Package integration contains integration tests that exercise real Postgres
// and Redis backends. Run with:
//
//	go test -tags integration ./test/integration/...
//
// Services must be running. Use the docker-compose.yml in this directory or
// the GitHub Actions services: block in .github/workflows/integration.yml.
package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/briefstore"
)

const (
	defaultPGDSN    = "postgres://kyber:test@localhost:5433/kyber?sslmode=disable"
	defaultRedisAddr = "localhost:6380"
	testAPIKey      = "integration-test-key"
	testNamespace   = "kyber-system"
)

// sharedDB is the shared database connection for integration tests.
// Initialised by TestMain in each test file's package.
var sharedDB *sql.DB

// sharedRDB is the shared Redis client for integration tests.
var sharedRDB *redis.Client

// pgDSN returns the Postgres DSN from env or default.
func pgDSN() string {
	if v := os.Getenv("KYBER_PG_DSN"); v != "" {
		return v
	}
	return defaultPGDSN
}

// redisAddr returns the Redis address from env or default.
func redisAddr() string {
	if v := os.Getenv("KYBER_REDIS_ADDR"); v != "" {
		return v
	}
	return defaultRedisAddr
}

// openDB opens a Postgres database connection using the environment-configured DSN.
func openDB() (*sql.DB, error) {
	db, err := sql.Open("postgres", pgDSN())
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	return db, nil
}

// openRedis creates a Redis client using the environment-configured address.
func openRedis() *redis.Client {
	return redis.NewClient(&redis.Options{Addr: redisAddr()})
}

// cleanRedisKey deletes a Redis key so a test starts from a clean slate.
func cleanRedisKey(t *testing.T, key string) {
	t.Helper()
	if err := sharedRDB.Del(context.Background(), key).Err(); err != nil {
		t.Fatalf("cleaning redis key %q: %v", key, err)
	}
}

// waitForPostgres polls until Postgres is ready or the deadline is exceeded.
func waitForPostgres(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		if err := db.PingContext(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("postgres did not become ready within timeout")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// waitForRedis polls until Redis is ready or the deadline is exceeded.
func waitForRedis(rdb *redis.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		if err := rdb.Ping(ctx).Err(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("redis did not become ready within timeout")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// runMigrations runs the BriefStore schema migration against the shared DB.
func runMigrations(db *sql.DB) error {
	store := briefstore.NewPostgresStore(db)
	return store.Migrate(context.Background())
}

// newTestScheme builds a runtime.Scheme with CRD types registered.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kyberv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme kyberv1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	return scheme
}

// authedRequest builds an HTTP request with the test API key set.
func authedRequest(t *testing.T, method, target string, body interface{}) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encoding request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, target, &buf)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	return req
}
