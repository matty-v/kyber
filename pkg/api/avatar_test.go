package api

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestValidatedAvatarType(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.White)
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		t.Fatal(err)
	}
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, img, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, contentType string
		data              []byte
		want              bool
	}{
		{"png", "image/png", pngBytes.Bytes(), true},
		{"jpeg", "image/jpeg", jpegBytes.Bytes(), true},
		{"mismatched", "image/png", jpegBytes.Bytes(), false},
		{"malformed", "image/jpeg", []byte("not an image"), false},
		{"webp malformed", "image/webp", []byte("RIFFxxxxNOPE"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validatedAvatarType(tc.data, tc.contentType); got != tc.want {
				t.Fatalf("validatedAvatarType() = %v, want %v", got, tc.want)
			}
		})
	}
	webp := make([]byte, 20)
	copy(webp, []byte("RIFF"))
	binary.LittleEndian.PutUint32(webp[4:8], 12)
	copy(webp[8:], []byte("WEBPVP8 "))
	if !validatedAvatarType(webp, "image/webp") {
		t.Fatal("valid WebP container was rejected")
	}
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func opaqueImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	return img
}

// withEXIF splices an APP1 Exif segment (a GPS-bearing one, in real photos)
// right after the JPEG's SOI marker.
func withEXIF(jpg []byte) []byte {
	payload := append([]byte("Exif\x00\x00"), []byte("GPS-SECRET-LOCATION")...)
	seg := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	out = append(out, payload...)
	return append(out, jpg[2:]...)
}

func TestNormalizeAvatar(t *testing.T) {
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, opaqueImage(800, 400), nil); err != nil {
		t.Fatal(err)
	}
	exif := withEXIF(jpg.Bytes())
	if !bytes.Contains(exif, []byte("GPS-SECRET")) {
		t.Fatal("fixture lacks EXIF")
	}
	out, ct, err := normalizeAvatar(exif, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || ct != "image/jpeg" || format != "jpeg" || cfg.Width != avatarSize || cfg.Height != avatarSize {
		t.Fatalf("normalized = %s %s %dx%d (%v), want a %dpx square JPEG", ct, format, cfg.Width, cfg.Height, err, avatarSize)
	}
	if bytes.Contains(out, []byte("Exif")) || bytes.Contains(out, []byte("GPS-SECRET")) {
		t.Fatal("EXIF survived normalization")
	}

	// Transparency keeps PNG; a small image is cropped square, never upscaled.
	clear := image.NewRGBA(image.Rect(0, 0, 50, 80))
	out, ct, err = normalizeAvatar(encodePNG(t, clear), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _, _ = image.DecodeConfig(bytes.NewReader(out)); ct != "image/png" || cfg.Width != 50 || cfg.Height != 50 {
		t.Fatalf("transparent small image = %s %dx%d, want 50x50 PNG", ct, cfg.Width, cfg.Height)
	}

	// WebP decodes (a 1x1 lossless image).
	webp, _ := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if _, ct, err = normalizeAvatar(webp, "image/webp"); err != nil {
		t.Fatalf("webp: %v", err)
	}

	// A header claiming huge dimensions is refused before decoding.
	bomb := encodePNG(t, image.NewGray(image.Rect(0, 0, 1, 1)))
	binary.BigEndian.PutUint32(bomb[16:20], 50000)
	binary.BigEndian.PutUint32(bomb[20:24], 50000)
	binary.BigEndian.PutUint32(bomb[29:33], crc32.ChecksumIEEE(bomb[12:29]))
	if _, _, err := normalizeAvatar(bomb, "image/png"); !errors.Is(err, errAvatarDimensions) {
		t.Fatalf("bomb = %v, want errAvatarDimensions", err)
	}
	if _, _, err := normalizeAvatar([]byte("nope"), "image/png"); !errors.Is(err, errAvatarType) {
		t.Fatalf("garbage = %v, want errAvatarType", err)
	}
}

func TestProfileAvatarURLIsVersioned(t *testing.T) {
	key := avatarKey("alice", []byte("x"))
	if got := profileAvatarURL("alice", key); got != "/api/v1/agents/alice/profile/avatar?v="+avatarVersion(key) || avatarVersion(key) == "" {
		t.Fatalf("url = %q", got)
	}
	if got := profileAvatarURL("alice", "agent-avatars/alice"); got != "/api/v1/agents/alice/profile/avatar" {
		t.Fatalf("legacy url = %q", got)
	}
}
