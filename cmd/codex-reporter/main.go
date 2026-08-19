package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/matty-v/kyber/pkg/tokenreport"
)

func main() {
	home, model := os.Getenv("HOME"), os.Getenv("CODEX_MODEL")
	if home == "" {
		log.Fatal("codex-reporter: HOME is required")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	root := filepath.Join(home, ".codex", "sessions")

	// ---- Credential syncer (kyber#681) ----
	// Watches ~/.codex/auth.json and pushes CLI-performed refreshes back to the
	// <name>-codex-auth Secret. ChatGPT refresh tokens are single use, so
	// without this the Secret holds an already-burnt token and the agent is
	// permanently unauthenticated after the next boot — see the type doc.
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	credSyncer := &tokenreport.CodexCredentialSyncer{
		AuthPath:    filepath.Join(codexHome, "auth.json"),
		SidecarURL:  os.Getenv("KYBER_SIDECAR_URL"),
		PushInitial: os.Getenv("KYBER_CODEX_PUSH_INITIAL") == "1",
		// fsnotify is the primary trigger; this is the missed-event backstop.
		Interval: 5 * time.Minute,
	}
	go credSyncer.Run(ctx)
	log.Printf("codex-reporter: credential syncer started (watching %s)", credSyncer.AuthPath)
	var lastActivity string
	var lastActivityPath string
	var lastActivityMod time.Time
	var lastTokens time.Time
	var lastCatalog time.Time
	bootActivity := true
	for {
		// Honour the signal context installed above. signal.NotifyContext
		// replaces SIGTERM's default disposition, so without this check the
		// reporter would stop dying on SIGTERM and instead linger until the
		// pod's grace period expired and SIGKILL landed.
		select {
		case <-ctx.Done():
			log.Print("codex-reporter: shutting down")
			return
		default:
		}
		// The full-tree scan reads every rollout file end to end, so it runs
		// exactly once at boot to recover the last-known state across restarts.
		// Every later tick uses the single-newest-file path, and only when that
		// file actually changed — otherwise this loop would re-read the whole
		// session history once per second for the life of the pod.
		state, at := tokenreport.ActivityUnknown, time.Time{}
		var activityErr error
		if bootActivity {
			state, at, activityErr = tokenreport.DetectCodexActivity(root)
		} else if path := latest(root); path != "" {
			info, statErr := os.Stat(path)
			if statErr == nil && (path != lastActivityPath || info.ModTime().After(lastActivityMod)) {
				state, at, activityErr = tokenreport.DetectCodexActivityFile(path)
				lastActivityPath, lastActivityMod = path, info.ModTime()
			}
		}
		bootActivity = false
		if activityErr == nil && state != tokenreport.ActivityUnknown && state != lastActivity {
			body, _ := json.Marshal(map[string]string{
				"type": "activity", "state": state, "at": at.UTC().Format(time.RFC3339),
			})
			if post(client, "event", body) == nil {
				lastActivity = state
			}
		}
		if time.Since(lastTokens) >= 30*time.Second {
			path := latest(root)
			if path != "" {
				snap, err := tokenreport.ParseCodexLatest(path, model)
				if err == nil && snap != nil {
					body, _ := json.Marshal(snap)
					_ = post(client, "token-usage", body)
				}
			}
			lastTokens = time.Now()
		}
		if time.Since(lastCatalog) >= time.Hour {
			if models, err := discoverModels(); err != nil {
				log.Printf("codex-reporter: model catalog discovery failed: %v", err)
			} else if body, err := json.Marshal(map[string]any{"runtime": "codex", "models": models}); err == nil {
				if err := post(client, "runtime-catalog", body); err != nil {
					log.Printf("codex-reporter: model catalog report failed: %v", err)
				}
			}
			lastCatalog = time.Now()
		}
		time.Sleep(time.Second)
	}
}

type catalogModel struct {
	ID                 string `json:"id"`
	DisplayName        string `json:"displayName"`
	ContextWindow      int64  `json:"contextWindow"`
	ContextWindowKnown bool   `json:"contextWindowKnown"`
}

// discoverModels uses the supported Codex app-server protocol rather than a
// hard-coded OpenAI list. app-server reads the same CODEX_HOME auth.json as the
// interactive harness, so model/list is subscription-aware.
func discoverModels() ([]catalogModel, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "app-server", "--listen", "stdio://")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("opening app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opening app-server stdout: %w", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting app-server: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Kill()
		select {
		case <-waitCh:
		case <-time.After(time.Second):
		}
	}()
	enc := json.NewEncoder(stdin)
	if err := enc.Encode(map[string]any{"method": "initialize", "id": 1, "params": map[string]any{"clientInfo": map[string]string{"name": "kyber", "title": "Kyber", "version": "1"}}}); err != nil {
		return nil, fmt.Errorf("writing app-server initialize: %w", err)
	}
	type response struct {
		ID     int `json:"id"`
		Result struct {
			Data []struct {
				ID                 string `json:"id"`
				Model              string `json:"model"`
				DisplayName        string `json:"displayName"`
				Hidden             bool   `json:"hidden"`
				ContextWindow      int64  `json:"contextWindow"`
				ContextWindowKnown bool   `json:"contextWindowKnown"`
			} `json:"data"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	initialized := false
	for scanner.Scan() {
		var resp response
		if json.Unmarshal(scanner.Bytes(), &resp) != nil {
			continue
		}
		if resp.ID == 1 && !initialized {
			if len(resp.Error) > 0 && string(resp.Error) != "null" {
				return nil, fmt.Errorf("app-server initialize returned %s", resp.Error)
			}
			if err := enc.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
				return nil, fmt.Errorf("writing app-server initialized: %w", err)
			}
			if err := enc.Encode(map[string]any{"method": "model/list", "id": 2, "params": map[string]any{"limit": 100, "includeHidden": false}}); err != nil {
				return nil, fmt.Errorf("writing app-server model/list: %w", err)
			}
			initialized = true
			continue
		}
		if resp.ID != 2 {
			continue
		}
		if len(resp.Error) > 0 && string(resp.Error) != "null" {
			return nil, fmt.Errorf("app-server model/list returned %s", resp.Error)
		}
		models := make([]catalogModel, 0, len(resp.Result.Data))
		for _, item := range resp.Result.Data {
			id := item.Model
			if id == "" {
				id = item.ID
			}
			if id == "" || item.Hidden {
				continue
			}
			if !item.ContextWindowKnown || item.ContextWindow <= 0 {
				return nil, fmt.Errorf("model %q omitted authoritative context-window metadata", id)
			}
			models = append(models, catalogModel{ID: id, DisplayName: item.DisplayName, ContextWindow: item.ContextWindow, ContextWindowKnown: true})
		}
		return models, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading app-server response: %w", err)
	}
	return nil, fmt.Errorf("app-server closed before model/list response")
}

func post(client *http.Client, endpoint string, body []byte) error {
	resp, err := client.Post("http://127.0.0.1:8091/"+endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s returned %s", endpoint, resp.Status)
	}
	return nil
}

func latest(root string) string {
	var best string
	var bestTime time.Time
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		info, err := d.Info()
		if err == nil && (best == "" || info.ModTime().After(bestTime)) {
			best, bestTime = path, info.ModTime()
		}
		return nil
	})
	return best
}
