package reasoningeffort

import "regexp"

var imageSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)
var imageDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

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
	return false
}

// ImageSupported either identifies the image built from this integration's
// source or reuses this Agent's previously observed integration for the exact
// immutable image. It never infers launch wiring from the native CLI version.
func ImageSupported(runtime, image, buildSHA, observedImage string) bool {
	repo := nativeImageRepository(runtime)
	if repo != "" && imageSHA.MatchString(buildSHA) && image == repo+":"+buildSHA {
		return true
	}
	return ImmutableNativeImage(runtime, image) && observedImage == image
}
