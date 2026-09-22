package tokenreport

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func writeCatalogFixture(t *testing.T, providerCache, metadata string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	providerPath := filepath.Join(dir, "provider_models_cache.json")
	metadataPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(providerPath, []byte(providerCache), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	return providerPath, metadataPath
}

// Reading the literal "openrouter" meant an agent on its own endpoint got an
// empty model picker even when the endpoint listed its models perfectly well.
func TestLoadHermesCatalogReadsTheActiveProvider(t *testing.T) {
	provider, metadata := writeCatalogFixture(t,
		`{"openrouter":{"models":["vendor/a"]},"kyber-endpoint":{"models":["qwen3.6-35b-a3b"]}}`,
		`{}`)

	models, err := LoadHermesCatalog(provider, metadata, "kyber-endpoint", 0, 100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(models) != 1 || models[0].ID != "qwen3.6-35b-a3b" {
		t.Fatalf("models = %+v, want the custom endpoint's model", models)
	}
}

// Metadata is OpenRouter-specific and authoritative only for OpenRouter. A
// self-hosted endpoint publishes none, so dropping models without it returned
// an empty catalog. For a non-OpenRouter provider they are kept with the
// context window marked unknown — a flag the API already carries end to end.
func TestLoadHermesCatalogKeepsModelsWithoutMetadata(t *testing.T) {
	provider, metadata := writeCatalogFixture(t,
		`{"kyber-endpoint":{"models":["qwen3.6-35b-a3b"]}}`,
		`{}`)

	models, err := LoadHermesCatalog(provider, metadata, "kyber-endpoint", 0, 100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %+v, want one entry", models)
	}
	if models[0].ContextWindowKnown {
		t.Error("context window must be reported unknown when there is no metadata")
	}
	if models[0].ContextWindow != 0 {
		t.Errorf("context window = %d, want 0 rather than a guess", models[0].ContextWindow)
	}
	// With no display name available, the id is the only honest label.
	if models[0].DisplayName != "qwen3.6-35b-a3b" {
		t.Errorf("display name = %q", models[0].DisplayName)
	}
}

// The OpenRouter path must be unchanged: real context windows, marked known.
func TestLoadHermesCatalogStillEnrichesOpenRouter(t *testing.T) {
	provider, metadata := writeCatalogFixture(t,
		`{"openrouter":{"models":["vendor/a"]}}`,
		`{"vendor/a":{"name":"Vendor A","context_length":128000}}`)

	models, err := LoadHermesCatalog(provider, metadata, "openrouter", 0, 100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %+v", models)
	}
	if !models[0].ContextWindowKnown || models[0].ContextWindow != 128000 {
		t.Errorf("context window = %d known=%v, want 128000/true", models[0].ContextWindow, models[0].ContextWindowKnown)
	}
	if models[0].DisplayName != "Vendor A" {
		t.Errorf("display name = %q, want Vendor A", models[0].DisplayName)
	}
}

// An empty provider keeps the historical default so an older sidecar that does
// not set HERMES_PROVIDER still reports the OpenRouter catalog.
func TestLoadHermesCatalogDefaultsToOpenRouter(t *testing.T) {
	provider, metadata := writeCatalogFixture(t,
		`{"openrouter":{"models":["vendor/a"]}}`,
		`{"vendor/a":{"name":"Vendor A","context_length":128000}}`)

	models, err := LoadHermesCatalog(provider, metadata, "", 0, 100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(models) != 1 || models[0].ID != "vendor/a" {
		t.Fatalf("models = %+v, want the OpenRouter catalog", models)
	}
}

// The OpenRouter drop-on-missing-metadata rule is deliberate and unchanged:
// there, metadata IS authoritative, so an id it does not describe is stale.
func TestLoadHermesCatalogStillDropsUnknownOpenRouterModels(t *testing.T) {
	provider, metadata := writeCatalogFixture(t,
		`{"openrouter":{"models":["vendor/a","vendor/stale"]}}`,
		`{"vendor/a":{"name":"Vendor A","context_length":128000}}`)

	models, err := LoadHermesCatalog(provider, metadata, "openrouter", 0, 100)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(models) != 1 || models[0].ID != "vendor/a" {
		t.Fatalf("models = %+v, want the stale OpenRouter id dropped", models)
	}
}

// A custom endpoint never talks to OpenRouter, so nothing writes its metadata
// cache. Erroring on the missing file returned an error every poll and left
// the model picker empty — the failure this path exists to remove.
func TestLoadHermesCatalogToleratesMissingMetadataForACustomProvider(t *testing.T) {
	dir := t.TempDir()
	providerPath := filepath.Join(dir, "provider_models_cache.json")
	if err := os.WriteFile(providerPath, []byte(`{"kyber-endpoint":{"models":["qwen3.6-35b-a3b"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	models, err := LoadHermesCatalog(providerPath, filepath.Join(dir, "absent.json"), "kyber-endpoint", 0, 100)
	if err != nil {
		t.Fatalf("a missing metadata cache must not fail a custom endpoint: %v", err)
	}
	if len(models) != 1 || models[0].ID != "qwen3.6-35b-a3b" {
		t.Fatalf("models = %+v, want the endpoint's model", models)
	}
	if models[0].ContextWindowKnown {
		t.Error("context window must be reported unknown")
	}
}

// For OpenRouter the file IS expected. Returning an empty catalog there would
// publish nothing over a previously good one, so a missing file stays an error.
func TestLoadHermesCatalogStillErrorsOnMissingOpenRouterMetadata(t *testing.T) {
	dir := t.TempDir()
	providerPath := filepath.Join(dir, "provider_models_cache.json")
	if err := os.WriteFile(providerPath, []byte(`{"openrouter":{"models":["vendor/a"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadHermesCatalog(providerPath, filepath.Join(dir, "absent.json"), "openrouter", 0, 100)
	if err == nil {
		t.Fatal("a missing OpenRouter metadata cache must still be an error")
	}
	// The reporter distinguishes "absent" from "broken" with errors.Is, which
	// only works if the wrap is preserved.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error must unwrap to fs.ErrNotExist, got %v", err)
	}
}
