package api

import (
	"bytes"
	"encoding/binary"
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
