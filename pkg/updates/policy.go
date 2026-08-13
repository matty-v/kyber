// Package updates implements Kyber's operator-facing update checking: what
// version this cluster runs, what version is available, and what the operator
// wants done about it.
//
// Design decisions worth knowing before changing anything here:
//
//   - The policy lives in a ConfigMap the CONTROL PLANE owns, and the Helm
//     chart deliberately does not template it. Every other operator-editable
//     setting is chart-seeded, which means a `helm upgrade` renders over it —
//     a bug we had to fix separately (kyber#41). The policy that governs
//     upgrades must not depend on the upgrade mechanism preserving it, and the
//     `lookup`-based fix that protects the others does nothing under ArgoCD's
//     repo-server. Keeping this resource out of the chart entirely removes the
//     whole class of problem.
//
//   - Applying an update is NOT implemented here. This package checks and
//     reports; it never mutates the cluster. `auto` is rejected at validation
//     until the apply path exists, because a mode that silently does nothing
//     is worse than one that refuses.
//
// See dave-agent spec 2026-08-10-kyber-owns-its-deployment.md.
package updates

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultConfigMapName is the ConfigMap the policy lives in. Created on first
// write by the control plane; absent means "never configured", which reads as
// the default policy rather than an error.
const DefaultConfigMapName = "kyber-update-policy"

// ConfigMap keys. Stable contract shared by the API handler and the PWA.
const (
	KeyChannel  = "channel"
	KeyMode     = "mode"
	KeyPinned   = "pinnedVersion"
	KeyWindow   = "window"
	KeyTimeZone = "timeZone"
)

// Channel selects which stream of builds this cluster watches.
type Channel string

const (
	// ChannelStable watches published GitHub Releases — the only channel with
	// a chart artifact today, because release.yml publishes the chart on tag.
	ChannelStable Channel = "stable"

	// ChannelMain watches head-of-main: every merge, published as a
	// pre-release chart by build.yml. This is the canary's channel and
	// deliberately nobody else's — a cluster on main moves on unreviewed code,
	// which is precisely what razer was taken off.
	ChannelMain Channel = "main"
)

// Accepts reports whether a version may be offered on this channel.
//
// This is the guard that keeps head-of-main out of production, and it lives
// here rather than in the parser because the same version string is correct on
// the canary and wrong everywhere else. A stable cluster never sees a
// pre-release; a main cluster sees both, because a freshly cut release IS
// newer than the main builds that preceded it and refusing it would strand the
// canary behind a version it should take.
func (c Channel) Accepts(v Version) bool {
	if c == ChannelMain {
		return true
	}
	return !v.IsPrerelease()
}

// Mode selects what happens when an update is found.
type Mode string

const (
	// ModeManual — check and record, surface nothing prominent.
	ModeManual Mode = "manual"
	// ModeNotify — report that an update is available. The default.
	ModeNotify Mode = "notify"
	// ModeAuto — apply it. Rejected at validation until the apply path lands.
	ModeAuto Mode = "auto"
)

// Policy is the operator's stated intent.
type Policy struct {
	Channel Channel `json:"channel"`
	Mode    Mode    `json:"mode"`

	// PinnedVersion holds this cluster at an exact version. Non-empty
	// overrides Channel and Mode entirely — a pin means "do not move".
	PinnedVersion string `json:"pinnedVersion,omitempty"`

	// Window and TimeZone bound when an automatic apply may START. They are
	// accepted and round-tripped now so the PWA can be built against the real
	// contract, but nothing reads them until the apply path exists.
	Window   string `json:"window,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

// DefaultPolicy is what an unconfigured cluster reports: watch stable releases,
// tell the operator, change nothing. Notify rather than manual because a
// cluster that silently knows it is out of date and says nothing is the worst
// of the three.
func DefaultPolicy() Policy {
	return Policy{Channel: ChannelStable, Mode: ModeNotify}
}

// Validate rejects anything this build cannot honour. The guiding rule: never
// accept a setting we will not act on. A stored policy that lies about what
// the cluster will do is worse than a rejected write.
func (p Policy) Validate() error {
	switch p.Channel {
	case ChannelStable, ChannelMain:
	case "":
		return fmt.Errorf("channel is required (%s)", strings.Join(validChannels(), ", "))
	default:
		return fmt.Errorf("unknown channel %q, want one of: %s", p.Channel, strings.Join(validChannels(), ", "))
	}

	switch p.Mode {
	case ModeManual, ModeNotify:
	case ModeAuto:
		return fmt.Errorf("mode %q is not available yet: this build can check for updates but cannot apply them, so %q would silently do nothing. Use %q", ModeAuto, ModeAuto, ModeNotify)
	case "":
		return fmt.Errorf("mode is required (%s)", strings.Join(validModes(), ", "))
	default:
		return fmt.Errorf("unknown mode %q, want one of: %s", p.Mode, strings.Join(validModes(), ", "))
	}

	if p.PinnedVersion != "" {
		pinned, err := ParseVersion(p.PinnedVersion)
		if err != nil {
			return fmt.Errorf("pinnedVersion: %w", err)
		}
		// A pin the channel would never offer is a contradiction the operator
		// should hear about now, not discover as a cluster that silently never
		// moves. Pinning a head-of-main build on the release channel is the
		// case that matters.
		if !p.Channel.Accepts(pinned) {
			return fmt.Errorf(
				"pinnedVersion %s is a pre-release and the %q channel takes published releases only; "+
					"pin a release, or switch the channel to %q",
				pinned, p.Channel, ChannelMain)
		}
	}
	return nil
}

func validChannels() []string { return []string{string(ChannelStable), string(ChannelMain)} }
func validModes() []string    { return []string{string(ModeManual), string(ModeNotify)} }

// Store reads and writes the policy ConfigMap.
type Store struct {
	Client        client.Client
	Namespace     string
	ConfigMapName string
}

func (s *Store) name() string {
	if s.ConfigMapName != "" {
		return s.ConfigMapName
	}
	return DefaultConfigMapName
}

// Load returns the stored policy, or DefaultPolicy when the ConfigMap does not
// exist. A missing ConfigMap is the normal state of a cluster nobody has
// configured — not an error to surface.
//
// Unknown or malformed stored values fall back to the default for that field
// rather than failing the read: an operator must always be able to SEE the
// current state in order to correct it, and a 500 on the status endpoint would
// hide exactly the thing they need to fix.
func (s *Store) Load(ctx context.Context) (Policy, error) {
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: s.name()}
	if err := s.Client.Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return DefaultPolicy(), nil
		}
		return DefaultPolicy(), err
	}
	p := DefaultPolicy()
	if v := cm.Data[KeyChannel]; v != "" {
		if Channel(v) == ChannelStable || Channel(v) == ChannelMain {
			p.Channel = Channel(v)
		}
	}
	if v := cm.Data[KeyMode]; v != "" {
		switch Mode(v) {
		case ModeManual, ModeNotify, ModeAuto:
			p.Mode = Mode(v)
		}
	}
	p.PinnedVersion = cm.Data[KeyPinned]
	p.Window = cm.Data[KeyWindow]
	p.TimeZone = cm.Data[KeyTimeZone]
	return p, nil
}

// Save writes the policy, creating the ConfigMap when absent. Callers must
// Validate first; Save does not, so a future apply-capable build can persist
// modes this one rejects without a store change.
func (s *Store) Save(ctx context.Context, p Policy) error {
	data := map[string]string{
		KeyChannel:  string(p.Channel),
		KeyMode:     string(p.Mode),
		KeyPinned:   p.PinnedVersion,
		KeyWindow:   p.Window,
		KeyTimeZone: p.TimeZone,
	}
	existing := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: s.name()}
	err := s.Client.Get(ctx, key, existing)
	switch {
	case err == nil:
		before := existing.DeepCopy()
		if existing.Data == nil {
			existing.Data = map[string]string{}
		}
		for k, v := range data {
			existing.Data[k] = v
		}
		return s.Client.Patch(ctx, existing, client.MergeFrom(before))
	case apierrors.IsNotFound(err):
		created := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      s.name(),
				Namespace: s.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name":       "kyber",
					"app.kubernetes.io/component":  "control-plane",
					"app.kubernetes.io/managed-by": "kyber-control-plane",
				},
			},
			Data: data,
		}
		if createErr := s.Client.Create(ctx, created); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				// Lost a create race. Patch the existing object directly
				// rather than recursing into Save.
				//
				// Recursing was unbounded: reads come from the manager's
				// cached client, so the Get at the top can keep returning
				// NotFound for as long as the informer lags behind the Create
				// that just landed — NotFound → Create → AlreadyExists →
				// recurse, spinning with no backoff and no attempt counter.
				// A second policy edit inside the cache-lag window of the
				// first-ever write is enough to trigger it.
				live := &corev1.ConfigMap{}
				if getErr := s.Client.Get(ctx, key, live); getErr != nil {
					return fmt.Errorf("policy ConfigMap already exists but could not be read back: %w", getErr)
				}
				before := live.DeepCopy()
				if live.Data == nil {
					live.Data = map[string]string{}
				}
				for k, v := range data {
					live.Data[k] = v
				}
				return s.Client.Patch(ctx, live, client.MergeFrom(before))
			}
			return createErr
		}
		return nil
	default:
		return err
	}
}
