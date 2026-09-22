package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/matty-v/kyber/pkg/tokenreport"
)

const (
	defaultReportInterval = 30 * time.Second
	catalogInterval       = time.Hour
	// catalogBackoffAfter is how many consecutive catalog rejections are
	// tolerated at the fast cadence before dropping to the hourly one. A
	// REJECTED catalog is a configuration problem, not a transient: resending
	// it every 30 seconds never succeeds and only floods the log.
	catalogBackoffAfter = 3
)

func main() {
	home := os.Getenv("HERMES_HOME")
	if home == "" {
		if userHome := os.Getenv("HOME"); userHome != "" {
			home = filepath.Join(userHome, ".hermes")
		}
	}
	if home == "" {
		log.Fatal("hermes-reporter: HERMES_HOME or HOME is required")
	}
	interval := defaultReportInterval
	if value := os.Getenv("KYBER_TOKEN_REPORT_INTERVAL"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			log.Fatalf("hermes-reporter: invalid KYBER_TOKEN_REPORT_INTERVAL %q", value)
		}
		interval = parsed
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	logPath := filepath.Join(home, "logs", "agent.log")
	providerCache := filepath.Join(home, "provider_models_cache.json")
	// Report the catalog for the provider this agent actually runs against.
	// HERMES_PROVIDER is "openrouter" for a default agent and the Kyber-managed
	// id for one pointed at its own inference endpoint; reading the literal
	// "openrouter" there returned an empty picker.
	activeProvider := os.Getenv("HERMES_PROVIDER")
	metadataCache := filepath.Join(home, "cache", "openrouter_model_metadata.json")

	// A self-hosted endpoint publishes no OpenRouter metadata, so ask it what
	// window it is serving. Resolved once at startup and refreshed only when
	// it is still unknown: the value changes when the operator restarts the
	// server, not minute to minute.
	endpointBase := os.Getenv("KYBER_INFERENCE_BASE_URL")
	endpointKey := os.Getenv("OPENAI_API_KEY")
	var endpointContext int64
	resolveEndpointContext := func() {
		if endpointBase == "" || endpointContext > 0 {
			return
		}
		probeCtx, cancelProbe := context.WithTimeout(ctx, 20*time.Second)
		defer cancelProbe()
		window, err := tokenreport.EndpointContextWindow(probeCtx, client, endpointBase, endpointKey)
		if err != nil {
			log.Printf("hermes-reporter: could not read the endpoint's context window: %v", err)
			return
		}
		if window <= 0 {
			log.Printf("hermes-reporter: %s does not publish a context window; the model picker and context budget stay empty", endpointBase)
			return
		}
		endpointContext = window
		log.Printf("hermes-reporter: endpoint context window = %d", window)
	}
	resolveEndpointContext()

	report := func() {
		snap, err := tokenreport.ParseHermesLatest(logPath, metadataCache, endpointContext)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("hermes-reporter: token discovery failed: %v", err)
			}
			return
		}
		if snap == nil {
			return
		}
		body, err := json.Marshal(snap)
		if err == nil {
			if err := post(ctx, client, "token-usage", body); err != nil {
				log.Printf("hermes-reporter: token report failed: %v", err)
			}
		}
	}
	reportCatalog := func() bool {
		resolveEndpointContext()
		models, err := tokenreport.LoadHermesCatalog(providerCache, metadataCache, activeProvider, endpointContext, 100)
		if err != nil {
			// errors.Is, not os.IsNotExist: the loader wraps with %w and
			// os.IsNotExist does not unwrap, so a simply-absent cache logged
			// a failure on every interval.
			if !errors.Is(err, fs.ErrNotExist) {
				log.Printf("hermes-reporter: model catalog discovery failed: %v", err)
			}
			return false
		}
		body, err := json.Marshal(map[string]any{"runtime": "hermes", "models": models})
		if err == nil {
			if err := post(ctx, client, "runtime-catalog", body); err != nil {
				log.Printf("hermes-reporter: model catalog report failed: %v", err)
				return false
			}
		}
		return true
	}

	report()
	lastCatalog := time.Time{}
	lastCatalogAttempt := time.Now()
	catalogFailures := 0
	if reportCatalog() {
		lastCatalog = time.Now()
	} else {
		catalogFailures++
	}
	tokenTicker := time.NewTicker(interval)
	defer tokenTicker.Stop()
	log.Printf("hermes-reporter: started home=%s interval=%s", home, interval)
	for {
		select {
		case <-ctx.Done():
			log.Print("hermes-reporter: shutting down")
			return
		case <-tokenTicker.C:
			report()
			// Fresh Hermes homes populate their native caches after this
			// process starts, so retry at the ordinary cadence until the first
			// success, then refresh hourly. Once the failures stop looking
			// like a cache that has not appeared yet, back off — an agent
			// whose catalog the control plane rejects logged a failure every
			// 30 seconds indefinitely and told the operator nothing new after
			// the first one.
			due := lastCatalog.IsZero() || time.Since(lastCatalog) >= catalogInterval
			if due && catalogFailures >= catalogBackoffAfter {
				due = time.Since(lastCatalogAttempt) >= catalogInterval
			}
			if due {
				lastCatalogAttempt = time.Now()
				if reportCatalog() {
					lastCatalog = time.Now()
					catalogFailures = 0
				} else {
					catalogFailures++
					if catalogFailures == catalogBackoffAfter {
						log.Printf("hermes-reporter: model catalog rejected %d times in a row; backing off to hourly", catalogFailures)
					}
				}
			}
		}
	}
}

func post(ctx context.Context, client *http.Client, endpoint string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:8091/"+endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s returned %s", endpoint, resp.Status)
	}
	return nil
}
