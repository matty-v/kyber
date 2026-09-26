package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matty-v/kyber/pkg/api"
	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/taskobject"
)

func testPNG(t *testing.T, c color.Color, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type avatarHarness struct {
	t      *testing.T
	h      http.Handler
	c      client.Client
	legacy *taskobject.MemoryStore
}

func newAvatarHarness(t *testing.T, legacyKey string) *avatarHarness {
	t.Helper()
	agent := sampleAgentCRD("alice")
	agent.UID = "alice-uid"
	agent.Spec.Profile.AvatarKey = legacyKey
	if legacyKey != "" {
		agent.Spec.Profile.AvatarContentType = "image/png"
	}
	c := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent, defaultMachine()).Build()
	legacy := taskobject.NewMemoryStore()
	s := &api.Server{K8sClient: c, APIKey: testAPIKey, Namespace: "kyber-system", TaskObjectStore: legacy}
	return &avatarHarness{t: t, h: s.BuildHandler(), c: c, legacy: legacy}
}

func (a *avatarHarness) do(method, target, contentType string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rr := httptest.NewRecorder()
	a.h.ServeHTTP(rr, req)
	return rr
}

func (a *avatarHarness) agent() *kyberv1.Agent {
	ag := &kyberv1.Agent{}
	if err := a.c.Get(context.Background(), types.NamespacedName{Name: "alice", Namespace: "kyber-system"}, ag); err != nil {
		a.t.Fatal(err)
	}
	return ag
}

func (a *avatarHarness) avatarURL(rr *httptest.ResponseRecorder) string {
	var v struct {
		Profile struct {
			AvatarURL string `json:"avatarUrl"`
		} `json:"profile"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &v)
	return v.Profile.AvatarURL
}

// MAT-91: with no object store at all, an avatar is stored in a ConfigMap
// the Agent owns, served under a versioned URL, replaced, and removed.
func TestAvatarRoundTripWithoutObjectStore(t *testing.T) {
	agent := sampleAgentCRD("alice")
	agent.UID = "alice-uid"
	c := fake.NewClientBuilder().WithScheme(mustNewScheme(t)).WithObjects(agent, defaultMachine()).Build()
	a := &avatarHarness{t: t, c: c, h: (&api.Server{K8sClient: c, APIKey: testAPIKey, Namespace: "kyber-system"}).BuildHandler()}

	rr := a.do(http.MethodPut, "/api/v1/agents/alice/profile/avatar", "image/png", testPNG(t, color.RGBA{200, 10, 10, 255}, 600, 300))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rr.Code, rr.Body.String())
	}
	url1 := a.avatarURL(rr)
	if !strings.HasPrefix(url1, "/api/v1/agents/alice/profile/avatar?v=") {
		t.Fatalf("avatarUrl = %q, want a versioned URL", url1)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "alice-avatar", Namespace: "kyber-system"}, cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != "alice-uid" || cm.OwnerReferences[0].Kind != "Agent" {
		t.Errorf("avatar ConfigMap owner = %+v", cm.OwnerReferences)
	}
	if ag := a.agent(); !strings.HasPrefix(ag.Spec.Profile.AvatarKey, "configmap:alice-avatar#") || ag.Spec.Profile.AvatarContentType != "image/jpeg" {
		t.Errorf("profile = %+v", ag.Spec.Profile)
	}

	get := a.do(http.MethodGet, url1, "", nil)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(get.Body.Bytes()))
	if get.Code != http.StatusOK || err != nil || format != "jpeg" || cfg.Width != 256 || cfg.Height != 256 {
		t.Fatalf("GET = %d %s %dx%d %v", get.Code, format, cfg.Width, cfg.Height, err)
	}
	if cc := get.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("versioned GET Cache-Control = %q", cc)
	}
	etag := get.Header().Get("ETag")
	if rr := a.do(http.MethodGet, "/api/v1/agents/alice/profile/avatar", "", nil, "If-None-Match", etag); rr.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", rr.Code)
	}
	if cc := a.do(http.MethodGet, "/api/v1/agents/alice/profile/avatar", "", nil).Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("unversioned GET must revalidate, got %q", cc)
	}

	// Replacing it changes the URL, so no browser keeps the old image.
	rr = a.do(http.MethodPut, "/api/v1/agents/alice/profile/avatar", "image/png", testPNG(t, color.RGBA{10, 10, 200, 255}, 64, 64))
	if url2 := a.avatarURL(rr); rr.Code != http.StatusOK || url2 == url1 {
		t.Fatalf("replace = %d, url %q (was %q)", rr.Code, url2, url1)
	}

	if rr := a.do(http.MethodDelete, "/api/v1/agents/alice/profile/avatar", "", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", rr.Code)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "alice-avatar", Namespace: "kyber-system"}, &corev1.ConfigMap{}); !k8serrors.IsNotFound(err) {
		t.Errorf("avatar ConfigMap left after DELETE: %v", err)
	}
	if rr := a.do(http.MethodGet, "/api/v1/agents/alice/profile/avatar", "", nil); rr.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d", rr.Code)
	}
}

func TestAvatarRejectsBadUploads(t *testing.T) {
	a := newAvatarHarness(t, "")
	for _, tc := range []struct {
		name, ct string
		body     []byte
		want     int
	}{
		{"wrong type", "image/gif", []byte("GIF89a"), http.StatusUnsupportedMediaType},
		{"lying type", "image/jpeg", testPNG(t, color.White, 4, 4), http.StatusUnsupportedMediaType},
		{"too large", "image/png", make([]byte, 1<<20+1), http.StatusRequestEntityTooLarge},
	} {
		if rr := a.do(http.MethodPut, "/api/v1/agents/alice/profile/avatar", tc.ct, tc.body); rr.Code != tc.want {
			t.Errorf("%s = %d, want %d: %s", tc.name, rr.Code, tc.want, rr.Body.String())
		}
	}
	if a.agent().Spec.Profile.AvatarKey != "" {
		t.Error("a rejected upload set an avatar")
	}
}

// An avatar stored in the object store before MAT-91 still serves, moves to
// a ConfigMap on the next upload, and is cleaned up when replaced or when
// its agent is deleted.
func TestAvatarLegacyObjectStore(t *testing.T) {
	a := newAvatarHarness(t, "agent-avatars/alice")
	old := testPNG(t, color.Black, 8, 8)
	if err := a.legacy.Put(context.Background(), "agent-avatars/alice", bytes.NewReader(old), int64(len(old)), taskobject.PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	if rr := a.do(http.MethodGet, "/api/v1/agents/alice/profile/avatar", "", nil); rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), old) {
		t.Fatalf("legacy GET = %d", rr.Code)
	}
	if rr := a.do(http.MethodPut, "/api/v1/agents/alice/profile/avatar", "image/png", testPNG(t, color.White, 8, 8)); rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d", rr.Code)
	}
	if _, err := a.legacy.Open(context.Background(), "agent-avatars/alice", nil); err == nil {
		t.Error("the replaced legacy object was kept")
	}

	b := newAvatarHarness(t, "agent-avatars/alice")
	if err := b.legacy.Put(context.Background(), "agent-avatars/alice", bytes.NewReader(old), int64(len(old)), taskobject.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if rr := b.do(http.MethodDelete, "/api/v1/agents/alice?confirm=alice", "", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete agent = %d %s", rr.Code, rr.Body.String())
	}
	if _, err := b.legacy.Open(context.Background(), "agent-avatars/alice", nil); err == nil {
		t.Error("the deleted agent's legacy avatar was kept")
	}
}
