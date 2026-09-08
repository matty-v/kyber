package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	metadataCache := filepath.Join(home, "cache", "openrouter_model_metadata.json")

	report := func() {
		snap, err := tokenreport.ParseHermesLatest(logPath, metadataCache)
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
		models, err := tokenreport.LoadHermesCatalog(providerCache, metadataCache, 100)
		if err != nil {
			if !os.IsNotExist(err) {
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
	if reportCatalog() {
		lastCatalog = time.Now()
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
			// Fresh Hermes homes populate their native caches after this process
			// starts. Retry with the ordinary report cadence until the first
			// successful catalog, then refresh hourly.
			if lastCatalog.IsZero() || time.Since(lastCatalog) >= catalogInterval {
				if reportCatalog() {
					lastCatalog = time.Now()
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
