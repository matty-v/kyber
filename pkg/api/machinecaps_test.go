package api

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestDefaultGCEVMTypeCatalog_KnownType(t *testing.T) {
	cat := DefaultGCEVMTypeCatalog()
	got, ok := cat["n1-standard-4"]
	if !ok {
		t.Fatal("DefaultGCEVMTypeCatalog: n1-standard-4 missing")
	}
	if got.CPU.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("cpu = %s, want 4", got.CPU.String())
	}
	if got.Memory.Cmp(resource.MustParse("15Gi")) != 0 {
		t.Errorf("memory = %s, want 15Gi", got.Memory.String())
	}
}

func TestDefaultGCEVMTypeCatalog_UnknownType(t *testing.T) {
	cat := DefaultGCEVMTypeCatalog()
	if _, ok := cat["xyz-massive-999"]; ok {
		t.Fatal("DefaultGCEVMTypeCatalog: xyz-massive-999 should not exist")
	}
}

func TestDefaultGCEVMTypeCatalog_AllDefaultsParse(t *testing.T) {
	cat := DefaultGCEVMTypeCatalog()
	for typ, cap := range cat {
		if cap.CPU.IsZero() {
			t.Errorf("type %s: cpu is zero", typ)
		}
		if cap.Memory.IsZero() {
			t.Errorf("type %s: memory is zero", typ)
		}
	}
}

func TestParseVMTypeCatalog_Valid(t *testing.T) {
	const input = `[{"type":"e2-small","cpu":"2","memory":"2Gi"},{"type":"e2-medium","cpu":"2","memory":"4Gi"}]`
	cat, err := ParseVMTypeCatalog(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cat) != 2 {
		t.Fatalf("want 2 entries, got %d", len(cat))
	}
	e2m, ok := cat["e2-medium"]
	if !ok {
		t.Fatal("e2-medium missing from parsed catalog")
	}
	if e2m.Memory.Cmp(resource.MustParse("4Gi")) != 0 {
		t.Errorf("e2-medium memory = %s, want 4Gi", e2m.Memory.String())
	}
}

func TestParseVMTypeCatalog_Invalid(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"bad json", `not-json`},
		{"empty array", `[]`},
		{"bad cpu", `[{"type":"e2-small","cpu":"bad","memory":"2Gi"}]`},
		{"bad memory", `[{"type":"e2-small","cpu":"2","memory":"bad"}]`},
		{"missing type", `[{"type":"","cpu":"2","memory":"2Gi"}]`},
		{"duplicate type", `[{"type":"e2-small","cpu":"2","memory":"2Gi"},{"type":"e2-small","cpu":"4","memory":"4Gi"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseVMTypeCatalog(tc.input); err == nil {
				t.Errorf("expected error for input %q", tc.input)
			}
		})
	}
}

func TestDefaultGCEVMTypeCatalog_ReturnsFreshCopy(t *testing.T) {
	a := DefaultGCEVMTypeCatalog()
	b := DefaultGCEVMTypeCatalog()
	// Mutating one copy must not affect the other.
	delete(a, "e2-small")
	if _, ok := b["e2-small"]; !ok {
		t.Error("mutating one returned map corrupted a second call — DefaultGCEVMTypeCatalog must return independent copies")
	}
}
