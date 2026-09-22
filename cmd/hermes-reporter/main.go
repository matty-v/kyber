package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
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
	// A dedicated client. The shared one above has a 5s Timeout, and
	// http.Client.Timeout bounds the whole request regardless of the request
	// context — so a context.WithTimeout here would be dead code, and a cold
	// self-hosted endpoint (the first call through the tunnel is slow by
	// design) would fail the probe every time and leave the window unknown.
	probeClient := &http.Client{Timeout: 30 * time.Second}
	var endpointWindows map[string]int64
	resolveEndpointContext := func() {
		if endpointBase == "" {
			return
		}
		probeCtx, cancelProbe := context.WithTimeout(ctx, 30*time.Second)
		defer cancelProbe()
		windows, err := tokenreport.EndpointContextWindow(probeCtx, probeClient, endpointBase, endpointKey)
		if err != nil {
			log.Printf("hermes-reporter: could not read the endpoint's context window: %v", err)
			return
		}
		if len(windows) == 0 {
			log.Printf("hermes-reporter: %s does not publish a context window; the model picker and context budget stay empty", endpointBase)
			return
		}
		// Re-resolved on every catalog refresh, not cached for the pod's
		// lifetime: an operator restarting llama.cpp with a different -c is
		// exactly the workflow this feature serves, and a stale window would
		// keep being published as authoritative until the pod restarted.
		if !maps.Equal(endpointWindows, windows) {
			log.Printf("hermes-reporter: endpoint context windows = %v", windows)
		}
		endpointWindows = windows
	}
	resolveEndpointContext()

	report := func() {
		snap, err := tokenreport.ParseHermesLatest(logPath, metadataCache, endpointWindows)
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
	// reportCatalog returns (reported, rejected). Only a control-plane
	// REJECTION counts toward the backoff: a provider cache that has not
	// appeared yet is the ordinary fresh-home case — the reporter is started
	// BEFORE Hermes itself — and backing off on it would leave the picker
	// empty and the budget "-" for up to an hour on every new agent.
	reportCatalog := func() (bool, bool) {
		resolveEndpointContext()
		// Hermes keys the cache by "custom:<base_url>" for a base_url provider
		// and by name for a built-in one, so try both. Getting this wrong meant
		// an endpoint agent posted an EMPTY catalog, which the control plane
		// rejects outright — the picker stayed empty and the reporter retried
		// until it backed off.
		providerKeys := []string{activeProvider}
		if endpointBase != "" {
			providerKeys = append(providerKeys, tokenreport.HermesCustomProviderKey(endpointBase))
		}
		models, err := tokenreport.LoadHermesCatalog(providerCache, metadataCache, providerKeys, endpointWindows, 100)
		if err != nil {
			// errors.Is, not os.IsNotExist: the loader wraps with %w and
			// os.IsNotExist does not unwrap, so a simply-absent cache logged
			// a failure on every interval.
			if !errors.Is(err, fs.ErrNotExist) {
				log.Printf("hermes-reporter: model catalog discovery failed: %v", err)
			}
			return false, false
		}
		body, err := json.Marshal(map[string]any{"runtime": "hermes", "models": models})
		if err != nil {
			return false, false
		}
		if err := post(ctx, client, "runtime-catalog", body); err != nil {
			log.Printf("hermes-reporter: model catalog report failed: %v", err)
			return false, true
		}
		return true, false
	}

	report()
	lastCatalog := time.Time{}
	lastCatalogAttempt := time.Now()
	catalogRejections := 0
	if ok, rejected := reportCatalog(); ok {
		lastCatalog = time.Now()
	} else if rejected {
		catalogRejections++
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
			if due && catalogRejections >= catalogBackoffAfter {
				due = time.Since(lastCatalogAttempt) >= catalogInterval
			}
			if due {
				lastCatalogAttempt = time.Now()
				ok, rejected := reportCatalog()
				switch {
				case ok:
					lastCatalog = time.Now()
					catalogRejections = 0
				case rejected:
					catalogRejections++
					if catalogRejections == catalogBackoffAfter {
						log.Printf("hermes-reporter: model catalog rejected %d times in a row; backing off to hourly", catalogRejections)
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
