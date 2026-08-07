package usersecrets

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr error
	}{
		// valid
		{"single uppercase letter", "A", nil},
		{"letters only", "FOO", nil},
		{"letters and digits", "FOO123", nil},
		{"letters and underscore", "FOO_BAR", nil},
		{"trailing digits", "APP_PEM_V2", nil},
		{"max length", strings.Repeat("A", MaxKeyLength), nil},

		// invalid — empty
		{"empty", "", ErrKeyEmpty},

		// invalid — length
		{"one over max", strings.Repeat("A", MaxKeyLength+1), ErrKeyTooLong},

		// invalid — grammar
		{"lowercase", "foo", ErrKeyBadGrammar},
		{"mixed case", "FooBar", ErrKeyBadGrammar},
		{"leading digit", "1FOO", ErrKeyBadGrammar},
		{"leading underscore", "_FOO", ErrKeyBadGrammar},
		{"hyphen", "FOO-BAR", ErrKeyBadGrammar},
		{"dot", "FOO.BAR", ErrKeyBadGrammar},
		{"space", "FOO BAR", ErrKeyBadGrammar},
		{"unicode", "FOO€", ErrKeyBadGrammar},

		// invalid — reserved prefixes
		{"reserved KYBER_", "KYBER_FOO", ErrKeyReservedPrefix},
		{"reserved USER_", "USER_FOO", ErrKeyReservedPrefix},
		{"reserved KYBER_ exact", "KYBER_", ErrKeyReservedPrefix},
		{"reserved USER_ exact", "USER_", ErrKeyReservedPrefix},

		// the bare prefix word without underscore is *not* reserved — only the
		// prefix-including-underscore form is.
		{"bare KYBER allowed", "KYBER", nil},
		{"bare USER allowed", "USER", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateKey(tc.key)
			if !errors.Is(got, tc.wantErr) {
				t.Errorf("ValidateKey(%q) = %v, want %v", tc.key, got, tc.wantErr)
			}
		})
	}
}

func TestValidateEntrySize(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		wantErr error
	}{
		{"zero", 0, nil},
		{"small", 1024, nil},
		{"exact max", MaxEntryBytes, nil},
		{"one over", MaxEntryBytes + 1, ErrValueTooLarge},
		{"much larger", MaxEntryBytes * 10, ErrValueTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateEntrySize(tc.size)
			if !errors.Is(got, tc.wantErr) {
				t.Errorf("ValidateEntrySize(%d) = %v, want %v", tc.size, got, tc.wantErr)
			}
		})
	}
}

func TestValidateAggregate(t *testing.T) {
	tests := []struct {
		name    string
		total   int
		wantErr error
	}{
		{"zero", 0, nil},
		{"small", 1024, nil},
		{"exact max", MaxAggregateBytes, nil},
		{"one over", MaxAggregateBytes + 1, ErrAggregateTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateAggregate(tc.total)
			if !errors.Is(got, tc.wantErr) {
				t.Errorf("ValidateAggregate(%d) = %v, want %v", tc.total, got, tc.wantErr)
			}
		})
	}
}
