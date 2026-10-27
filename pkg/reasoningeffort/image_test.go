package reasoningeffort

import (
	"strings"
	"testing"
)

func TestEffortImageIntegrationCannotBeInferredFromCLIVersion(t *testing.T) {
	sha := strings.Repeat("1", 40)
	old := strings.Repeat("2", 40)
	repo := "ghcr.io/matty-v/kyber-codex"
	for _, tc := range []struct {
		name, image, build, observed string
		want                         bool
	}{
		{"same qualified source", repo + ":" + sha, sha, "", true},
		{"old runtime image", repo + ":" + old, sha, "", false},
		{"own previously observed image", repo + ":" + old, sha, repo + ":" + old, true},
		{"different observed image", repo + ":" + old, sha, repo + ":" + sha, false},
		{"mutable tag", repo + ":latest", sha, repo + ":latest", false},
		{"fork", "example.com/fork:" + sha, sha, "example.com/fork:" + sha, false},
		{"observed digest", repo + "@sha256:" + strings.Repeat("a", 64), sha, repo + "@sha256:" + strings.Repeat("a", 64), true},
		{"missing build identity", repo + ":" + sha, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ImageSupported("codex", tc.image, tc.build, tc.observed); got != tc.want {
				t.Fatalf("supported=%v want=%v", got, tc.want)
			}
		})
	}
	if ImmutableNativeImage("hermes", repo+":"+sha) {
		t.Fatal("unqualified runtime inherited native integration")
	}
}
