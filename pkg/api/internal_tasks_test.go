package api

import (
	"bytes"
	"testing"
)

func TestCanonicalTaskJSONRejectsDuplicateKeysAndStabilizesOrder(t *testing.T) {
	if _, err := canonicalTaskJSON([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate key accepted")
	}
	one, err := canonicalTaskJSON([]byte(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	two, err := canonicalTaskJSON([]byte(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one, two) {
		t.Fatalf("canonical values differ: %s != %s", one, two)
	}
}

func TestParseTaskByteRange(t *testing.T) {
	r, partial, err := parseTaskByteRange("bytes=2-20", 6)
	if err != nil || !partial || r.Offset != 2 || r.Length != 4 {
		t.Fatalf("range=%+v partial=%v err=%v", r, partial, err)
	}
	r, partial, err = parseTaskByteRange("bytes=-2", 6)
	if err != nil || !partial || r.Offset != 4 || r.Length != 2 {
		t.Fatalf("suffix range=%+v partial=%v err=%v", r, partial, err)
	}
}

func TestReconcileTaskMediaTypeAcceptsWAVAliases(t *testing.T) {
	for _, declared := range []string{"audio/wav", "audio/x-wav", "audio/vnd.wave", "audio/wave"} {
		t.Run(declared, func(t *testing.T) {
			if got := reconcileTaskMediaType(declared, "audio/wave", "audio/wave"); got != "audio/wave" {
				t.Fatalf("reconcileTaskMediaType() = %q, want audio/wave", got)
			}
		})
	}
}
