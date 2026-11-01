package reasoningeffort

import (
	"regexp"
	"strings"
)

var imageSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)
var imageDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var sourceVersionTag = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+-[0-9]+-g([a-f0-9]{7,40})$`)

func nativeImageRepository(runtime string) string {
	switch runtime {
	case "claude-code":
		return "ghcr.io/matty-v/kyber-claude-code"
	case "codex":
		return "ghcr.io/matty-v/kyber-codex"
	default:
		return ""
	}
}

// ImmutableNativeImage accepts only first-party content/source-pinned references.
// A mutable tag or a fork needs separate integration qualification.
func ImmutableNativeImage(runtime, image string) bool {
	repo := nativeImageRepository(runtime)
	if repo == "" {
		return false
	}
	if len(image) == len(repo)+1+40 && image[:len(repo)+1] == repo+":" {
		return imageSHA.MatchString(image[len(repo)+1:])
	}
	if len(image) == len(repo)+8+64 && image[:len(repo)+8] == repo+"@sha256:" {
		return imageDigest.MatchString(image[len(repo)+8:])
	}
	if tag, ok := strings.CutPrefix(image, repo+":"); ok {
		return sourceVersionTag.MatchString(tag)
	}
	return false
}

// ImageSupported either identifies the image built from this integration's
// source or reuses this Agent's previously observed integration for the exact
// immutable image. It never infers launch wiring from the native CLI version.
func ImageSupported(runtime, image, buildSHA, observedImage string) bool {
	if SourceImageSupported(runtime, image, buildSHA) {
		return true
	}
	return ImmutableNativeImage(runtime, image) && observedImage == image
}

// SourceImageSupported accepts the current first-party source image, including
// the source-version tags emitted by Kyber's standalone release publisher.
// It intentionally does not reuse older images' protocol-one observations for
// the newer native-at-boot validation contract.
func SourceImageSupported(runtime, image, buildSHA string) bool {
	repo := nativeImageRepository(runtime)
	if repo == "" || !imageSHA.MatchString(buildSHA) {
		return false
	}
	if image == repo+":"+buildSHA {
		return true
	}
	tag, ok := strings.CutPrefix(image, repo+":")
	if !ok {
		return false
	}
	match := sourceVersionTag.FindStringSubmatch(tag)
	return len(match) == 2 && strings.HasPrefix(buildSHA, match[1])
}

// NativeImageSupported additionally accepts a previous protocol-two observation
// for this exact immutable image; protocol-one observations cannot qualify it.
func NativeImageSupported(runtime, image, buildSHA, observedNativeImage string) bool {
	return SourceImageSupported(runtime, image, buildSHA) || (runtime == "codex" && ImmutableNativeImage(runtime, image) && image == observedNativeImage)
}
