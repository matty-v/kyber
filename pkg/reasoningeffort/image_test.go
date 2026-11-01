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

func TestSourceVersionImageTags(t *testing.T) {
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	for _, tc := range []struct {
		tag  string
		want bool
	}{
		{"1.8.0-90-gabcdef0", true}, {"1.8.0-90-gabcdef01", true},
		{"1.8.0-90-g1234567", false}, {"1.8.0-90-gabcdef", false},
		{"latest", false}, {"1.8.0-90-gabcdef0-dirty", false},
	} {
		if got := SourceImageSupported("codex", "ghcr.io/matty-v/kyber-codex:"+tc.tag, sha); got != tc.want {
			t.Fatalf("tag %s: %v", tc.tag, got)
		}
	}
}
