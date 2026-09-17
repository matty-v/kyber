package credentialsync

import "testing"

func TestHashClaudeSeparatesFields(t *testing.T) {
	a := HashClaude("ab", "c", 12)
	b := HashClaude("a", "bc", 12)
	if a == b {
		t.Fatal("field boundaries must affect the credential hash")
	}
	if !ValidHash(a) {
		t.Fatalf("HashClaude returned invalid digest %q", a)
	}
}

func TestValidHash(t *testing.T) {
	valid := HashOpaque([]byte(`{"fixture":true}`))
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{valid, true},
		{"", false},
		{valid[:63], false},
		{"A" + valid[1:], false},
		{"z" + valid[1:], false},
	} {
		if got := ValidHash(tc.value); got != tc.want {
			t.Errorf("ValidHash(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
