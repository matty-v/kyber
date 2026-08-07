package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHealthzReflectsLoopLiveness_NotControlPlaneReachability is the kyber#575
// regression: the liveness endpoint must report healthy as long as the
// heartbeat loop is advancing, EVEN WHEN the control plane is unreachable
// (lastPostOK=false). The old behavior gated /healthz on lastPostOK, so a
// transient control-plane outage tripped the kubelet liveness probe and got
// every sidecar SIGTERM'd — and under restartPolicy: Never, they stayed dead.
func TestHealthzReflectsLoopLiveness_NotControlPlaneReachability(t *testing.T) {
	cases := []struct {
		name       string
		lastTick   time.Time // zero => never ticked
		lastPostOK bool
		want       int
	}{
		{
			name:       "loop fresh, control plane DOWN -> healthy (the #575 fix)",
			lastTick:   time.Now(),
			lastPostOK: false, // control plane unreachable must NOT make us unhealthy
			want:       http.StatusOK,
		},
		{
			name:       "loop fresh, control plane up -> healthy",
			lastTick:   time.Now(),
			lastPostOK: true,
			want:       http.StatusOK,
		},
		{
			name:       "loop just inside the staleness window -> healthy",
			lastTick:   time.Now().Add(-(livenessStaleThreshold - time.Second)),
			lastPostOK: true,
			want:       http.StatusOK,
		},
		{
			name:       "loop stalled past threshold -> unhealthy (genuine hang)",
			lastTick:   time.Now().Add(-(livenessStaleThreshold + time.Second)),
			lastPostOK: true, // even with a prior good post, a hung loop is dead
			want:       http.StatusServiceUnavailable,
		},
		{
			name:       "never ticked -> unhealthy",
			lastTick:   time.Time{},
			lastPostOK: true,
			want:       http.StatusServiceUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.lastTick.IsZero() {
				lastLoopTick.Store(0)
			} else {
				lastLoopTick.Store(tc.lastTick.UnixNano())
			}
			lastPostOK.Store(tc.lastPostOK)

			rec := httptest.NewRecorder()
			healthzHandler(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			if rec.Code != tc.want {
				t.Errorf("healthz code = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
