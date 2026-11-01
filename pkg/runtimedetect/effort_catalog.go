package runtimedetect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// EffortCatalog is current-pod evidence, separate from the picker cache. Never
// authorize an effort write from an unbound, another-pod or expired catalog.
type EffortCatalog struct {
	AgentUID   string    `json:"agentUID"`
	PodUID     string    `json:"podUID"`
	Runtime    string    `json:"runtime"`
	Version    string    `json:"version"`
	ObservedAt time.Time `json:"observedAt"`
	Models     []Model   `json:"models"`
}

const EffortCatalogMaxAge = 2 * time.Hour

type EffortCatalogCache interface {
	PutEffortCatalog(context.Context, string, EffortCatalog) error
	GetEffortCatalog(context.Context, string) (EffortCatalog, error)
}

func (m *MemoryCache) PutEffortCatalog(_ context.Context, name string, c EffortCatalog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.effortCatalogs == nil {
		m.effortCatalogs = map[string]EffortCatalog{}
	}
	c.Models = cloneModels(c.Models)
	m.effortCatalogs[name] = c
	return nil
}
func (m *MemoryCache) GetEffortCatalog(_ context.Context, name string) (EffortCatalog, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.effortCatalogs[name]
	if !ok {
		return c, ErrCacheEmpty
	}
	c.Models = cloneModels(c.Models)
	return c, nil
}
func (r *RedisCache) PutEffortCatalog(ctx context.Context, name string, c EffortCatalog) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshaling effort catalog: %w", err)
	}
	if err = r.client.Set(ctx, "runtimedetect:effort:"+name, raw, EffortCatalogMaxAge).Err(); err != nil {
		return fmt.Errorf("storing effort catalog: %w", err)
	}
	return nil
}
func (r *RedisCache) GetEffortCatalog(ctx context.Context, name string) (EffortCatalog, error) {
	var c EffortCatalog
	raw, err := r.client.Get(ctx, "runtimedetect:effort:"+name).Bytes()
	if errors.Is(err, redis.Nil) {
		return c, ErrCacheEmpty
	}
	if err != nil {
		return c, fmt.Errorf("getting effort catalog: %w", err)
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("decoding effort catalog: %w", err)
	}
	return c, nil
}
