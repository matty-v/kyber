package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestCallbackRegistryIsChatScopedOneShotAndBounded(t *testing.T) {
	r := newCallbackRegistry()
	_, tokens, err := r.register("42", []callbackButton{{Text: "Go", Value: "internal-value"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.consume(tokens[0], "99"); ok {
		t.Fatal("callback was consumable from the wrong chat")
	}
	entry, ok := r.consume(tokens[0], "42")
	if !ok || entry.Value != "internal-value" {
		t.Fatalf("entry=%+v ok=%v", entry, ok)
	}
	if _, ok := r.consume(tokens[0], "42"); ok {
		t.Fatal("callback was consumable twice")
	}

	var oldest string
	for i := 0; i <= maxTelegramCallbacks; i++ {
		_, generated, err := r.register("42", []callbackButton{{Text: "B", Value: fmt.Sprint(i)}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldest = generated[0]
		}
	}
	if _, ok := r.consume(oldest, "42"); ok {
		t.Fatal("oldest callback was not evicted")
	}
	if strings.Contains(oldest, "internal-value") {
		t.Fatal("callback token exposed its value")
	}
}

func TestCallbackRegistryRejectsUnboundedModelData(t *testing.T) {
	r := newCallbackRegistry()
	tests := []struct {
		name    string
		buttons []callbackButton
	}{
		{"too many buttons", make([]callbackButton, maxTelegramButtons+1)},
		{"label too long", []callbackButton{{Text: strings.Repeat("x", maxTelegramButtonRunes+1), Value: "v"}}},
		{"value too long", []callbackButton{{Text: "X", Value: strings.Repeat("v", maxTelegramCallbackValue+1)}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := r.register("42", tc.buttons); err == nil {
				t.Fatal("expected bounded callback validation error")
			}
		})
	}
}
