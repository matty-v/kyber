package updates

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNS = "kyber-system"

func newStore(objs ...client.Object) *Store {
	return &Store{
		Client:    fake.NewClientBuilder().WithObjects(objs...).Build(),
		Namespace: testNS,
	}
}

func policyCM(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultConfigMapName, Namespace: testNS},
		Data:       data,
	}
}

// An unconfigured cluster is the overwhelmingly common case. It must read as
// the default policy, not as an error — the update card has to render on a
// cluster nobody has touched.
func TestStore_LoadMissingConfigMapReturnsDefault(t *testing.T) {
	got, err := newStore().Load(context.Background())
	if err != nil {
		t.Fatalf("Load on a missing ConfigMap errored: %v", err)
	}
	if want := DefaultPolicy(); got != want {
		t.Errorf("Load = %+v, want the default %+v", got, want)
	}
}

func TestStore_SaveThenLoadRoundTrips(t *testing.T) {
	s := newStore()
	want := Policy{
		Channel:       ChannelStable,
		Mode:          ModeManual,
		PinnedVersion: "1.0.1",
		Window:        "0 2 * * *",
		TimeZone:      "America/Denver",
	}
	if err := s.Save(context.Background(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// Save must create the ConfigMap on a cluster that has never had one — the
// operator's first edit cannot require a chart change to land.
func TestStore_SaveCreatesConfigMapWhenAbsent(t *testing.T) {
	s := newStore()
	if err := s.Save(context.Background(), DefaultPolicy()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: testNS, Name: DefaultConfigMapName}
	if err := s.Client.Get(context.Background(), key, cm); err != nil {
		t.Fatalf("ConfigMap was not created: %v", err)
	}
	if cm.Data[KeyChannel] != string(ChannelStable) {
		t.Errorf("channel = %q, want %q", cm.Data[KeyChannel], ChannelStable)
	}
}

// Save patches rather than replaces, so keys written by a future build (or by
// an operator out of band) are not silently dropped by an older one.
func TestStore_SavePreservesUnknownKeys(t *testing.T) {
	s := newStore(policyCM(map[string]string{
		KeyChannel:      string(ChannelStable),
		"someFutureKey": "keep-me",
	}))
	if err := s.Save(context.Background(), Policy{Channel: ChannelStable, Mode: ModeNotify}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: testNS, Name: DefaultConfigMapName}
	if err := s.Client.Get(context.Background(), key, cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["someFutureKey"] != "keep-me" {
		t.Errorf("unknown key was dropped by Save; data = %v", cm.Data)
	}
}

// A stored value this build doesn't recognise must not break the read. The
// operator needs to see current state in order to fix it; a 500 on the status
// endpoint would hide the thing they need.
func TestStore_LoadFallsBackOnGarbageValues(t *testing.T) {
	s := newStore(policyCM(map[string]string{
		KeyChannel: "not-a-channel",
		KeyMode:    "not-a-mode",
	}))
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load errored on garbage values: %v", err)
	}
	if got.Channel != ChannelStable || got.Mode != ModeNotify {
		t.Errorf("Load = %+v, want the defaults for unrecognised values", got)
	}
}

func TestPolicy_ValidateAcceptsSupportedCombinations(t *testing.T) {
	for _, p := range []Policy{
		{Channel: ChannelStable, Mode: ModeNotify},
		{Channel: ChannelStable, Mode: ModeManual},
		{Channel: ChannelStable, Mode: ModeNotify, PinnedVersion: "1.0.1"},
		{Channel: ChannelStable, Mode: ModeNotify, PinnedVersion: "v1.0.1"},
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", p, err)
		}
	}
}

// The core honesty rule for this build: never accept a setting we won't act
// on. `auto` has no apply path yet, so accepting it would leave an operator
// believing their cluster self-updates when it does not — and they would find
// out by being months behind.
func TestPolicy_ValidateRejectsAutoUntilApplyExists(t *testing.T) {
	err := Policy{Channel: ChannelStable, Mode: ModeAuto}.Validate()
	if err == nil {
		t.Fatal("Validate accepted mode=auto, but nothing can apply an update in this build")
	}
	if !strings.Contains(err.Error(), "silently do nothing") {
		t.Errorf("error should explain WHY auto is refused; got %q", err)
	}
}

// The main channel is the canary's: build.yml publishes a chart per merge, so
// a cluster set to it has something real to find.
func TestPolicy_ValidateAcceptsMainChannel(t *testing.T) {
	if err := (Policy{Channel: ChannelMain, Mode: ModeNotify}).Validate(); err != nil {
		t.Fatalf("Validate rejected channel=main: %v", err)
	}
}

// A pin the channel would never offer is a contradiction, and saying so beats
// a cluster that silently never moves.
func TestPolicy_ValidateRejectsPrereleasePinOnStable(t *testing.T) {
	err := (Policy{Channel: ChannelStable, Mode: ModeNotify, PinnedVersion: "1.0.2-25-gfd47d00"}).Validate()
	if err == nil {
		t.Fatal("Validate accepted a head-of-main pin on the release channel")
	}
	if !strings.Contains(err.Error(), "pre-release") {
		t.Errorf("error should say why; got %q", err)
	}
	// The same pin is legitimate on the canary's channel.
	if err := (Policy{Channel: ChannelMain, Mode: ModeNotify, PinnedVersion: "1.0.2-25-gfd47d00"}).Validate(); err != nil {
		t.Errorf("Validate rejected a head-of-main pin on the main channel: %v", err)
	}
}

func TestPolicy_ValidateRejectsUnparseablePin(t *testing.T) {
	for _, pin := range []string{"latest", "1.2", "1.2.3-rc1", "main"} {
		if err := (Policy{Channel: ChannelStable, Mode: ModeNotify, PinnedVersion: pin}).Validate(); err == nil {
			t.Errorf("Validate accepted pinnedVersion=%q", pin)
		}
	}
}

func TestPolicy_ValidateRejectsEmptyFields(t *testing.T) {
	if err := (Policy{Mode: ModeNotify}).Validate(); err == nil {
		t.Error("Validate accepted an empty channel")
	}
	if err := (Policy{Channel: ChannelStable}).Validate(); err == nil {
		t.Error("Validate accepted an empty mode")
	}
}
