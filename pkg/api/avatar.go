package api

// avatar.go — agent profile avatars (MAT-91). An avatar is normalized on
// upload (square, at most avatarSize px, re-encoded so no metadata survives)
// and kept in a ConfigMap owned by its Agent: it needs no object store, so it
// works on every installation, and it is garbage-collected with the agent.
// The Agent records only an opaque key that carries the image's hash, which
// versions the avatar URL so browsers can cache it for good.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"strings"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers WebP decoding with image.Decode

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/taskobject"
)

const (
	// avatarSize is the side of the stored square avatar.
	avatarSize = 256
	// maxAvatarSourceSide bounds the decoded source image, checked from the
	// header before decoding so a tiny file cannot expand into gigabytes.
	maxAvatarSourceSide = 4096
	// maxStoredAvatarBytes bounds a normalized avatar. The control plane's
	// informer caches every ConfigMap in its namespace, so avatars stay small.
	maxStoredAvatarBytes = 256 << 10
	// avatarKeyPrefix marks keys stored by this file; any other key is a
	// legacy object-store key.
	avatarKeyPrefix = "configmap:"
	avatarDataKey   = "avatar"
	// legacyAvatarPrefix is the object-store key prefix used before MAT-91.
	legacyAvatarPrefix = "agent-avatars/"
)

var (
	errAvatarType       = errors.New("avatar must be a PNG, JPEG, or WebP image")
	errAvatarDimensions = fmt.Errorf("avatar must be at most %d×%d pixels", maxAvatarSourceSide, maxAvatarSourceSide)
	errAvatarNotFound   = errors.New("agent has no avatar")
)

func avatarConfigMapName(agent string) string { return agent + "-avatar" }

// avatarKey is the opaque key an Agent records for a stored avatar.
func avatarKey(agent string, data []byte) string {
	sum := sha256.Sum256(data)
	return avatarKeyPrefix + avatarConfigMapName(agent) + "#" + hex.EncodeToString(sum[:6])
}

// avatarVersion is the hash part of a key, or "" for a legacy key.
func avatarVersion(key string) string {
	if !strings.HasPrefix(key, avatarKeyPrefix) {
		return ""
	}
	_, v, _ := strings.Cut(key, "#")
	return v
}

// normalizeAvatar decodes an uploaded image, center-crops it square, scales
// it to at most avatarSize and re-encodes it: PNG when it has transparency,
// JPEG otherwise. Re-encoding drops EXIF and every other metadata block.
func normalizeAvatar(data []byte, declared string) ([]byte, string, error) {
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if !validatedAvatarType(data, declared) {
		return nil, "", errAvatarType
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", errAvatarType
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxAvatarSourceSide || cfg.Height > maxAvatarSourceSide {
		return nil, "", errAvatarDimensions
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", errAvatarType
	}
	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	crop := image.Rect(0, 0, side, side).Add(image.Pt(b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2))
	out := min(side, avatarSize)
	dst := image.NewRGBA(image.Rect(0, 0, out, out))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, crop, draw.Src, nil)

	var buf bytes.Buffer
	contentType := "image/jpeg"
	if dst.Opaque() {
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85})
	} else {
		contentType = "image/png"
		err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, dst)
	}
	if err != nil {
		return nil, "", fmt.Errorf("encoding avatar: %w", err)
	}
	if buf.Len() > maxStoredAvatarBytes {
		return nil, "", fmt.Errorf("normalized avatar is %d bytes, over %d", buf.Len(), maxStoredAvatarBytes)
	}
	return buf.Bytes(), contentType, nil
}

// avatarStore reads and writes avatars. Reader, when set, bypasses the
// informer cache so a just-stored avatar is served at once.
type avatarStore struct {
	Client    client.Client
	Reader    client.Reader
	Namespace string
	// Legacy is the object store avatars lived in before MAT-91; nil when the
	// installation has none.
	Legacy taskobject.ObjectStore
}

func (s avatarStore) reader() client.Reader {
	if s.Reader != nil {
		return s.Reader
	}
	return s.Client
}

// Load returns the agent's avatar bytes.
func (s avatarStore) Load(ctx context.Context, agent *kyberv1.Agent) ([]byte, error) {
	key := agent.Spec.Profile.AvatarKey
	switch {
	case key == "":
		return nil, errAvatarNotFound
	case strings.HasPrefix(key, avatarKeyPrefix):
		cm := &corev1.ConfigMap{}
		err := s.reader().Get(ctx, types.NamespacedName{Name: avatarConfigMapName(agent.Name), Namespace: s.Namespace}, cm)
		if k8serrors.IsNotFound(err) {
			return nil, errAvatarNotFound
		} else if err != nil {
			return nil, err
		}
		data, ok := cm.BinaryData[avatarDataKey]
		if !ok {
			return nil, errAvatarNotFound
		}
		return data, nil
	default:
		if s.Legacy == nil {
			return nil, errAvatarNotFound
		}
		obj, err := s.Legacy.Open(ctx, key, nil)
		if errors.Is(err, taskobject.ErrNotFound) {
			return nil, errAvatarNotFound
		} else if err != nil {
			return nil, err
		}
		defer obj.Body.Close()
		data, err := io.ReadAll(io.LimitReader(obj.Body, maxAgentAvatarBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxAgentAvatarBytes {
			return nil, fmt.Errorf("avatar exceeds %d bytes", maxAgentAvatarBytes)
		}
		return data, nil
	}
}

// Store writes a normalized avatar for the agent (name and UID) and returns
// the key and content type to record on it. The ConfigMap is owned by the
// Agent, so deleting the agent deletes it.
func (s avatarStore) Store(ctx context.Context, name string, uid types.UID, data []byte, contentType string) (string, error) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: avatarConfigMapName(name), Namespace: s.Namespace}}
	key := avatarKey(name, data)
	err := s.Client.Get(ctx, client.ObjectKeyFromObject(cm), cm)
	if err != nil && !k8serrors.IsNotFound(err) {
		return "", err
	}
	exists := err == nil
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	cm.Labels["app.kubernetes.io/managed-by"] = "kyber-api"
	cm.Labels["kyber.io/agent"] = name
	cm.Labels["kyber.io/component"] = "avatar"
	isController, block := true, true
	cm.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: kyberv1.GroupVersion.String(), Kind: "Agent", Name: name, UID: uid,
		Controller: &isController, BlockOwnerDeletion: &block,
	}}
	cm.Annotations = map[string]string{"kyber.io/avatar-content-type": contentType, "kyber.io/avatar-version": avatarVersion(key)}
	cm.BinaryData = map[string][]byte{avatarDataKey: data}
	cm.Data = nil
	if exists {
		err = s.Client.Update(ctx, cm)
	} else {
		err = s.Client.Create(ctx, cm)
	}
	if err != nil {
		return "", fmt.Errorf("storing avatar: %w", err)
	}
	return key, nil
}

// Remove deletes whatever an old key points at. Removing nothing is fine.
func (s avatarStore) Remove(ctx context.Context, agentName, key string) error {
	switch {
	case key == "":
		return nil
	case strings.HasPrefix(key, avatarKeyPrefix):
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: avatarConfigMapName(agentName), Namespace: s.Namespace}}
		if err := s.Client.Delete(ctx, cm); err != nil && !k8serrors.IsNotFound(err) {
			return err
		}
	case s.Legacy != nil:
		if err := s.Legacy.Delete(ctx, key); err != nil && !errors.Is(err, taskobject.ErrNotFound) {
			return err
		}
	}
	return nil
}
