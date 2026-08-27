package requeststore

import (
	"errors"
	"testing"
)

func TestRedisKeysUseAgentHashTag(t *testing.T) {
	request, outstanding, terminal, prefix := redisKeys("kiosk", "req_1")
	if request != "agentrequest:{kiosk}:request:req_1" ||
		outstanding != "agentrequest:{kiosk}:outstanding" ||
		terminal != "agentrequest:{kiosk}:terminal" ||
		prefix != "agentrequest:{kiosk}:request:" {
		t.Fatalf("redisKeys() = %q, %q, %q, %q", request, outstanding, terminal, prefix)
	}
}

func TestTransitionResult(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		runErr  error
		wantErr error
	}{
		{"success", int64(0), nil, nil},
		{"not found", int64(1), nil, ErrNotFound},
		{"conflict", int64(2), nil, ErrConflict},
		{"redis error", nil, errors.New("offline"), errors.New("offline")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := transitionResult("transitioning", tc.value, tc.runErr)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("transitionResult() error = %v", err)
			}
			if tc.wantErr != nil && tc.runErr == nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("transitionResult() error = %v, want %v", err, tc.wantErr)
			}
			if tc.runErr != nil && (err == nil || err.Error() != "transitioning: offline") {
				t.Fatalf("transitionResult() error = %v", err)
			}
		})
	}
}
