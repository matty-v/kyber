package tokenreport

import (
	"os"
	"path/filepath"
	"testing"
)

func writeHermesFixture(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseHermesLatest(t *testing.T) {
	logPath := writeHermesFixture(t, "agent.log", ""+
		"2026-09-08 13:05:00,303 INFO [session_one] agent.conversation_loop: API call #1: model=z-ai/glm-5.2 provider=openrouter in=17813 out=53 total=17866 latency=3.3s\n"+
		"2026-09-08 13:05:01,609 INFO [session_one] agent.conversation_loop: API call #2: model=z-ai/glm-5.2 provider=openrouter in=18320 out=57 total=18377 latency=1.2s cache=17792/18320 (97%)\n"+
		"2026-09-08 13:05:02,000 INFO [session_one] turn_context: user said secret text\n"+
		"2026-09-08 13:05:03,033 INFO [session_one] agent.conversation_loop: API call #3: model=z-ai/glm-5.2 provider=openrouter in=18393 out=22 total=18415 latency=1.1s cache=18304/18393 (100%)\n")
	metadata := writeHermesFixture(t, "metadata.json", `{"z-ai/glm-5.2":{"context_length":131072,"name":"GLM 5.2"}}`)
	snap, err := ParseHermesLatest(logPath, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil {
		t.Fatal("snapshot is nil")
	}
	if snap.Model != "z-ai/glm-5.2" || snap.Provider != "openrouter" {
		t.Fatalf("model/provider = %q/%q", snap.Model, snap.Provider)
	}
	if snap.Tokens.Used != 18393 || snap.Tokens.CacheRead != 18304 || snap.Tokens.Input != 89 {
		t.Fatalf("tokens = %+v", snap.Tokens)
	}
	if snap.Tokens.Output != 132 {
		t.Fatalf("output = %d, want cumulative 132", snap.Tokens.Output)
	}
	if snap.Tokens.Limit != 131072 || !snap.ContextWindowKnown {
		t.Fatalf("context = %d known=%v", snap.Tokens.Limit, snap.ContextWindowKnown)
	}
}

func TestParseHermesLatestIgnoresNonSummaryAndScopesOutput(t *testing.T) {
	logPath := writeHermesFixture(t, "agent.log", ""+
		"2026-09-08 13:05:00,303 INFO [old] agent.conversation_loop: API call #1: model=old/model provider=openrouter in=100 out=50 total=150 latency=1s\n"+
		"user payload API call #9: model=bad provider=bad in=999 out=999 total=1998 latency=1s\n"+
		"2026-09-08 13:06:00,303 INFO [new] agent.conversation_loop: API call #1: model=new/model provider=openrouter in=200 out=20 total=220 latency=1s\n")
	metadata := writeHermesFixture(t, "metadata.json", `{}`)
	snap, err := ParseHermesLatest(logPath, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Model != "new/model" || snap.Tokens.Output != 20 || snap.ContextWindowKnown {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestLoadHermesCatalog(t *testing.T) {
	providers := writeHermesFixture(t, "providers.json", `{"openrouter":{"models":["z/model","a/model","a/model","unknown"]},"other":{"models":["ignored"]}}`)
	metadata := writeHermesFixture(t, "metadata.json", `{"a/model":{"context_length":200000,"name":"A Model"},"z/model":{"context_length":1000000,"name":"Z Model"},"unknown":{"context_length":0}}`)
	models, err := LoadHermesCatalog(providers, metadata, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "a/model" || models[1].ID != "z/model" {
		t.Fatalf("models = %+v", models)
	}
	if !models[0].ContextWindowKnown || models[0].ContextWindow != 200000 {
		t.Fatalf("first model = %+v", models[0])
	}
}
