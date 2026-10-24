package operatorcredentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const fixtureAdminID = "credential_fixture_admin"
const fixtureServiceID = "credential_fixture_service"

func fixtureOperation(action, target string) Operation {
	return Operation{ID: uuid.NewString(), AdmissionEventID: uuid.NewString(), CompletionEventID: uuid.NewString(), RequestID: uuid.NewString(), Action: action, ActorPrincipalID: "principal_fixture_admin", ActorCredentialID: fixtureAdminID, ActorGeneration: 1, Authority: "configuration", TargetCredentialID: target, At: time.Date(2026, 10, 23, 12, 0, 0, 123456789, time.UTC), Status: "pending"}
}
func fixtureDocument() Document {
	return Document{SchemaVersion: SchemaVersion, Namespace: "kyber-system", Credentials: []Credential{
		(Credential{ID: fixtureAdminID, PrincipalID: "principal_fixture_admin", TenantID: "tenant_installation", Name: "Installation administrator", Role: RoleAdministrator, State: StateActive, Generation: 1, PermissionRevision: 1, Source: Source{Kind: "installation"}}).WithSecretMaterial([]byte("fixture-admin-material")),
		(Credential{ID: fixtureServiceID, PrincipalID: "principal_fixture_service", TenantID: "tenant_installation", Name: "Service integration", Role: RoleRestricted, State: StateActive, Generation: 1, PermissionRevision: 1, Scopes: []string{"metrics:read"}, Source: Source{Kind: "managed"}}).WithSecretMaterial([]byte("fixture-service-material")),
	}, Operations: []Operation{fixtureOperation("adopt", "")}}
}
func fixtureStore(t *testing.T) (*SecretStore, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	s, err := newSecretStore(c, c, "kyber-system", "registry-fixture")
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestPrivateCodecAndDiagnosticBoundary(t *testing.T) {
	d := fixtureDocument()
	material := []byte{0xff, 'x', 0x00, 'y'}
	d.Credentials[1] = d.Credentials[1].WithSecretMaterial(material)
	material[1] = 'z'
	raw, normalized, err := encode(d)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := decoded.Credentials[1].SecretMaterial()
	if !bytes.Equal(got, []byte{0xff, 'x', 0, 'y'}) {
		t.Fatal("binary material changed or aliased input")
	}
	got[1] = 'a'
	if decoded.Credentials[1].SecretMaterial()[1] != 'x' {
		t.Fatal("material accessor exposed mutable authority")
	}
	if !decoded.Operations[0].At.Equal(normalized.Operations[0].At) {
		t.Fatal("operation time changed")
	}
	for _, value := range []any{d, d.Credentials[0], Snapshot{Version: "42", Document: d}} {
		if _, err := json.Marshal(value); !errors.Is(err, ErrSensitive) {
			t.Fatal("public JSON accepted authority data")
		}
		for _, format := range []string{"%v", "%+v", "%#v", "%d", "%+d", "%q", "%x", "%#x"} {
			text := fmt.Sprintf(format, value)
			if !strings.Contains(text, "[redacted]") || strings.Contains(text, "fixture-admin-material") || strings.Contains(text, "principal_fixture") {
				t.Fatal("diagnostic formatting exposed authority data")
			}
		}
	}
}

func TestCodecRejectsAmbiguousAndUnsupportedDocuments(t *testing.T) {
	raw, _, err := encode(fixtureDocument())
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]byte{
		"duplicate": bytes.Replace(raw, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"SchemaVersion":1`), 1),
		"unknown":   bytes.Replace(raw, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"privateMarker":true`), 1),
		"trailing":  append(bytes.Clone(raw), []byte(`{}`)...),
		"malformed": []byte(`{"schemaVersion":1,"material":"private-marker"`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decode(bad); !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "private-marker") {
				t.Fatal("malformed authority was accepted or exposed")
			}
		})
	}
	if _, err := decode(bytes.Replace(raw, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":2`), 1)); !errors.Is(err, ErrUnsupported) {
		t.Fatal("future schema accepted")
	}
	if _, err := decode(make([]byte, MaxDocumentBytes+1)); !errors.Is(err, ErrCapacity) {
		t.Fatal("oversized document accepted")
	}
}

func TestModelBoundsAndAdministratorSafety(t *testing.T) {
	for name, change := range map[string]func(*Document){
		"blank material": func(d *Document) { d.Credentials[1] = d.Credentials[1].WithSecretMaterial([]byte(" \t\n")) },
		"duplicate material": func(d *Document) {
			d.Credentials[1] = d.Credentials[1].WithSecretMaterial(d.Credentials[0].SecretMaterial())
		},
		"duplicate identity":      func(d *Document) { d.Credentials[1].ID = d.Credentials[0].ID },
		"ambiguous name":          func(d *Document) { d.Credentials[1].Name = strings.ToUpper(d.Credentials[0].Name) },
		"legacy identity":         func(d *Document) { d.Credentials[1].Name = "legacy" },
		"cross namespace":         func(d *Document) { d.Credentials[1].AgentResources = []string{"other/alpha"} },
		"wildcard":                func(d *Document) { d.Credentials[1].AgentResources = []string{"*"} },
		"unknown actor":           func(d *Document) { d.Operations[0].ActorCredentialID = "credential_missing" },
		"future actor generation": func(d *Document) { d.Operations[0].ActorGeneration = 2 },
		"wrong principal":         func(d *Document) { d.Operations[0].ActorPrincipalID = "principal_other" },
		"unknown target":          func(d *Document) { d.Operations[0].TargetCredentialID = "credential_missing" },
		"event identity reuse":    func(d *Document) { d.Operations[0].CompletionEventID = d.Operations[0].AdmissionEventID },
	} {
		t.Run(name, func(t *testing.T) {
			d := fixtureDocument()
			change(&d)
			if _, _, err := encode(d); !errors.Is(err, ErrInvalid) {
				t.Fatalf("unsafe candidate accepted: %v", err)
			}
		})
	}
	d := fixtureDocument()
	expires := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	d.Credentials[0].ExpiresAt = &expires
	if _, _, err := encode(d); !errors.Is(err, ErrLastAdministrator) {
		t.Fatal("last administrator can expire")
	}
	d = fixtureDocument()
	d.Credentials[0].Source = Source{Kind: "external", MaterialRef: &SecretReference{Name: "external-material", Key: "token"}}
	if _, _, err := encode(d); !errors.Is(err, ErrLastAdministrator) {
		t.Fatal("last administrator depends on external material")
	}
	d = fixtureDocument()
	d.Credentials = make([]Credential, MaxCredentials+1)
	if _, _, err := encode(d); !errors.Is(err, ErrCapacity) {
		t.Fatal("credential count unbounded")
	}
	d = fixtureDocument()
	d.Credentials = d.Credentials[:1]
	for i := 0; i < 100; i++ {
		c := fixtureDocument().Credentials[1]
		c.ID = fmt.Sprintf("credential_%03d", i)
		c.Name = fmt.Sprintf("integration %03d", i)
		material := bytes.Repeat([]byte{'x'}, 4096)
		copy(material, fmt.Sprintf("fixture-%03d", i))
		d.Credentials = append(d.Credentials, c.WithSecretMaterial(material))
	}
	if _, _, err := encode(d); !errors.Is(err, ErrCapacity) {
		t.Fatal("document size unbounded")
	}
}

func TestStoreRejectsStaleWritesWithoutLosingMetadata(t *testing.T) {
	ctx := context.Background()
	a, c := fixtureStore(t)
	initial, err := a.Create(ctx, fixtureDocument())
	if err != nil || initial.Version == "" {
		t.Fatal("create", err)
	}
	b, err := newSecretStore(c, c, a.key.Namespace, a.key.Name)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := b.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next := normalized(initial.Document)
	next.Credentials[1].Name = "Renamed integration"
	next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
	updated, err := a.Replace(ctx, initial.Version, next)
	if err != nil || updated.Version == initial.Version {
		t.Fatal("CAS update", err)
	}
	stale.Document.Credentials[1].Name = "Stale integration"
	stale.Document.Operations = append(stale.Document.Operations, fixtureOperation("update", fixtureServiceID))
	if _, err := b.Replace(ctx, stale.Version, stale.Document); !errors.Is(err, ErrConflict) {
		t.Fatal("stale writer replaced state", err)
	}
	fresh, err := b.Load(ctx)
	if err != nil || fresh.Document.Credentials[1].Name != "Renamed integration" {
		t.Fatal("second reader cached stale state", err)
	}
	if _, err := a.Create(ctx, fixtureDocument()); !errors.Is(err, ErrConflict) {
		t.Fatal("create overwrote existing authority")
	}
}

func TestIdentityProvenanceCannotBeRewritten(t *testing.T) {
	for _, field := range []string{"kind", "issuer", "subject"} {
		t.Run(field, func(t *testing.T) {
			s, _ := fixtureStore(t)
			snapshot, err := s.Create(context.Background(), fixtureDocument())
			if err != nil {
				t.Fatal(err)
			}
			next := snapshot.Document.Clone()
			switch field {
			case "kind":
				next.Credentials[1].Kind = "service"
			case "issuer":
				next.Credentials[1].Issuer = "another-issuer"
			case "subject":
				next.Credentials[1].Subject = "another-subject"
			}
			next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
			if _, err = s.Replace(context.Background(), snapshot.Version, next); !errors.Is(err, ErrInvalid) {
				t.Fatal("identity provenance changed", err)
			}
		})
	}
}

func TestIdentityRevisionAndTombstoneTransitions(t *testing.T) {
	for name, change := range map[string]func(*Document){
		"principal": func(d *Document) { d.Credentials[1].PrincipalID = "another-principal" },
		"tenant":    func(d *Document) { d.Credentials[1].TenantID = "another-tenant" },
		"creation":  func(d *Document) { at := time.Now().UTC(); d.Credentials[1].CreatedAt = &at },
		"material without generation": func(d *Document) {
			d.Credentials[1] = d.Credentials[1].WithSecretMaterial([]byte("new-fixture-material"))
		},
		"grant without revision": func(d *Document) { d.Credentials[1].Scopes = []string{"logs:read"} },
		"cosmetic generation":    func(d *Document) { d.Credentials[1].Generation++ },
		"pending audit loss":     func(d *Document) { d.Operations = d.Operations[1:] },
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := fixtureStore(t)
			snapshot, err := s.Create(context.Background(), fixtureDocument())
			if err != nil {
				t.Fatal(err)
			}
			next := normalized(snapshot.Document)
			next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
			change(&next)
			if _, err = s.Replace(context.Background(), snapshot.Version, next); !errors.Is(err, ErrInvalid) {
				t.Fatal("unsafe transition accepted", err)
			}
		})
	}
	s, _ := fixtureStore(t)
	snapshot, err := s.Create(context.Background(), fixtureDocument())
	if err != nil {
		t.Fatal(err)
	}
	next := normalized(snapshot.Document)
	next.Credentials[1] = next.Credentials[1].WithSecretMaterial([]byte("rotated-fixture-material"))
	next.Credentials[1].Generation++
	next.Operations = append(next.Operations, fixtureOperation("rotate", fixtureServiceID))
	snapshot, err = s.Replace(context.Background(), snapshot.Version, next)
	if err != nil || snapshot.Document.Credentials[1].PermissionRevision != 1 {
		t.Fatal("rotation changed ownership/policy", err)
	}
	next = normalized(snapshot.Document)
	next.Credentials[1].State = StateRevoked
	next.Credentials[1] = next.Credentials[1].WithSecretMaterial(nil)
	next.Credentials[1].PermissionRevision++
	next.Operations = append(next.Operations, fixtureOperation("revoke", fixtureServiceID))
	snapshot, err = s.Replace(context.Background(), snapshot.Version, next)
	if err != nil {
		t.Fatal("revoke", err)
	}
	next = normalized(snapshot.Document)
	next.Credentials[1].State = StateActive
	next.Credentials[1] = next.Credentials[1].WithSecretMaterial([]byte("resurrected-fixture-material"))
	next.Credentials[1].PermissionRevision++
	next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
	if _, err = s.Replace(context.Background(), snapshot.Version, next); !errors.Is(err, ErrInvalid) {
		t.Fatal("revoked record revived", err)
	}
}

type failingReader struct {
	client.Reader
	failure error
	blocked bool
	calls   int
}

func (r *failingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.calls++
	if r.blocked {
		<-ctx.Done()
		return ctx.Err()
	}
	if r.failure != nil {
		return r.failure
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestAuthorityFailureHasNoCachedFallbackOrPrivateErrors(t *testing.T) {
	s, c := fixtureStore(t)
	if _, err := s.Create(context.Background(), fixtureDocument()); err != nil {
		t.Fatal(err)
	}
	r := &failingReader{Reader: c}
	s.reader = r
	if _, err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.failure = errors.New("private-material-marker")
	if _, err := s.Load(context.Background()); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "private-material-marker") {
		t.Fatal("cached state or private error returned")
	}
	r.failure = fmt.Errorf("private-material-marker: %w", context.Canceled)
	if _, err := s.Load(context.Background()); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "private-material-marker") {
		t.Fatal("wrapped cancellation leaked")
	}
	r.blocked = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.Load(ctx); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrUnavailable) {
		t.Fatal("deadline lost", err)
	}
}

func TestUnownedSecretIsNotAdopted(t *testing.T) {
	s, c := fixtureStore(t)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s.key.Name, Namespace: s.key.Namespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"unrelated": []byte("unrelated-fixture-value")}}
	if err := c.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatal("unowned authority accepted")
	}
	if _, err := s.Create(context.Background(), fixtureDocument()); !errors.Is(err, ErrConflict) {
		t.Fatal("unrelated Secret overwritten")
	}
	got := &corev1.Secret{}
	if err := c.Get(context.Background(), s.key, got); err != nil || string(got.Data["unrelated"]) != "unrelated-fixture-value" {
		t.Fatal("unrelated data changed")
	}
}

func TestConcurrentAdministratorsCannotRevokeEachOther(t *testing.T) {
	ctx := context.Background()
	s, _ := fixtureStore(t)
	d := fixtureDocument()
	d.Credentials[1].Role = RoleAdministrator
	d.Credentials[1].Scopes = nil
	base, err := s.Create(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	a, b := base.Document.Clone(), base.Document.Clone()
	a.Credentials[1].State = StateRevoked
	a.Credentials[1] = a.Credentials[1].WithSecretMaterial(nil)
	a.Credentials[1].PermissionRevision++
	a.Operations = append(a.Operations, fixtureOperation("revoke", fixtureServiceID))
	winner, err := s.Replace(ctx, base.Version, a)
	if err != nil {
		t.Fatal(err)
	}
	b.Credentials[0].State = StateRevoked
	b.Credentials[0] = b.Credentials[0].WithSecretMaterial(nil)
	b.Credentials[0].PermissionRevision++
	op := fixtureOperation("revoke", fixtureAdminID)
	op.ActorCredentialID = fixtureServiceID
	op.ActorPrincipalID = "principal_fixture_service"
	b.Operations = append(b.Operations, op)
	if _, err = s.Replace(ctx, base.Version, b); !errors.Is(err, ErrConflict) {
		t.Fatal("two stale administrator writes won", err)
	}
	if _, err = s.Replace(ctx, winner.Version, b); !errors.Is(err, ErrInvalid) {
		t.Fatal("fresh version alone revived a revoked administrator", err)
	}
	last := winner.Document.Clone()
	last.Credentials[0].State = StateRevoked
	last.Credentials[0] = last.Credentials[0].WithSecretMaterial(nil)
	last.Credentials[0].PermissionRevision++
	last.Operations = append(last.Operations, fixtureOperation("revoke", fixtureAdminID))
	if _, err = s.Replace(ctx, winner.Version, last); !errors.Is(err, ErrLastAdministrator) {
		t.Fatal("last administrator removed", err)
	}
}

func TestNewOperationCannotClaimRestrictedAdministrator(t *testing.T) {
	s, _ := fixtureStore(t)
	d := fixtureDocument()
	d.Operations[0].ActorCredentialID = fixtureServiceID
	d.Operations[0].ActorPrincipalID = "principal_fixture_service"
	if _, err := s.Create(context.Background(), d); !errors.Is(err, ErrInvalid) {
		t.Fatal("restricted record claimed administrator mutation", err)
	}
}

func TestPendingOperationMetadataAndAcknowledgement(t *testing.T) {
	ctx := context.Background()
	s, _ := fixtureStore(t)
	base, err := s.Create(ctx, fixtureDocument())
	if err != nil {
		t.Fatal(err)
	}
	bad := base.Document.Clone()
	bad.Operations[0].Action = "update"
	bad.Operations[0].TargetCredentialID = fixtureServiceID
	if _, err = s.Replace(ctx, base.Version, bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("audit metadata rewritten", err)
	}
	next := base.Document.Clone()
	next.Operations[0].Status = "delivered"
	ack, err := s.Replace(ctx, base.Version, next)
	if err != nil {
		t.Fatal("delivery acknowledgement", err)
	}
	bad = ack.Document.Clone()
	bad.Operations[0].Status = "pending"
	if _, err = s.Replace(ctx, ack.Version, bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("acknowledgement rolled back", err)
	}
	next = ack.Document.Clone()
	next.Operations = nil
	pruned, err := s.Replace(ctx, ack.Version, next)
	if err != nil || len(pruned.Document.Operations) != 0 {
		t.Fatal("delivered references could not be pruned", err)
	}
}

type racingWriter struct {
	client.Client
	updates int
}

func (w *racingWriter) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	w.updates++
	current := &corev1.Secret{}
	if err := w.Client.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
		return err
	}
	current.Annotations = map[string]string{"fixture-race": "retained"}
	if err := w.Client.Update(ctx, current); err != nil {
		return err
	}
	return w.Client.Update(ctx, obj, opts...)
}
func TestWriteRacingAuthoritativeReadIsNotRetried(t *testing.T) {
	ctx := context.Background()
	s, c := fixtureStore(t)
	base, err := s.Create(ctx, fixtureDocument())
	if err != nil {
		t.Fatal(err)
	}
	w := &racingWriter{Client: c}
	s.writer = w
	next := base.Document.Clone()
	next.Credentials[1].Name = "Conflicting edit"
	next.Operations = append(next.Operations, fixtureOperation("update", fixtureServiceID))
	if _, err = s.Replace(ctx, base.Version, next); !errors.Is(err, ErrConflict) || w.updates != 1 {
		t.Fatal("racing write was overwritten or retried", err)
	}
	fresh, err := s.Load(ctx)
	if err != nil || fresh.Document.Credentials[1].Name == "Conflicting edit" {
		t.Fatal("conflicting content committed", err)
	}
	secret := &corev1.Secret{}
	if err = c.Get(ctx, s.key, secret); err != nil || secret.Annotations["fixture-race"] != "retained" {
		t.Fatal("concurrent metadata lost")
	}
}

func TestPublicFactoryUsesOnlyDirectSecretRequests(t *testing.T) {
	raw, _, err := encode(fixtureDocument())
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := requests.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/kyber-system/secrets/registry-fixture" {
			t.Error("unexpected discovery or authority request")
			http.Error(w, "unsupported", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		secret := corev1.Secret{TypeMeta: metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: "registry-fixture", Namespace: "kyber-system", ResourceVersion: fmt.Sprint(number), Labels: map[string]string{ownerLabel: "v1", "app.kubernetes.io/managed-by": "kyber"}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{dataKey: raw}}
		if err := json.NewEncoder(w).Encode(secret); err != nil {
			t.Error("fixture response failed")
		}
	}))
	defer server.Close()
	config := &rest.Config{Host: server.URL, Timeout: 17 * time.Second}
	s, err := NewSecretStore(config, "kyber-system", "registry-fixture")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || a.Version == b.Version || config.Timeout != 17*time.Second {
		t.Fatal("authority cached, discovery performed, or caller config mutated")
	}
}

func TestCloneAndFullWidthGenerations(t *testing.T) {
	d := fixtureDocument()
	d.Credentials[1].Generation = ^uint64(0)
	d.Credentials[1].Source = Source{Kind: "external", MaterialRef: &SecretReference{Name: "external-material", Key: "token"}}
	raw, _, err := encode(d)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decode(raw)
	if err != nil || decoded.Credentials[1].Generation != ^uint64(0) {
		t.Fatal("generation lost precision", err)
	}
	clone := decoded.Clone()
	clone.Credentials[1].Source.MaterialRef.Name = "changed-ref"
	clone.Credentials[1].Scopes[0] = "logs:read"
	if decoded.Credentials[1].Source.MaterialRef.Name != "external-material" || decoded.Credentials[1].Scopes[0] != "metrics:read" {
		t.Fatal("clone shares nested state")
	}
	next := decoded.Clone()
	next.Credentials[1] = next.Credentials[1].WithSecretMaterial([]byte("rotated-fixture-material"))
	next.Credentials[1].Generation = 0
	next.Operations = append(next.Operations, fixtureOperation("rotate", fixtureServiceID))
	if _, _, err = encode(next); !errors.Is(err, ErrInvalid) {
		t.Fatal("generation wrapped")
	}
}

func TestLegacyBrowserSeedIsPrivateCopiedAndImmutable(t *testing.T) {
	seed := bytes.Repeat([]byte{42}, 32)
	document := fixtureDocument().WithLegacyBrowserSigningKey(seed, 1)
	document.Credentials[0] = document.Credentials[0].WithLegacyBrowserGeneration(1)
	seed[0] = 1
	if document.LegacyBrowserSigningKey()[0] != 42 {
		t.Fatal("seed aliases input")
	}
	exposed := document.LegacyBrowserSigningKey()
	exposed[0] = 2
	clone := document.Clone()
	clone.legacyBrowserSigningKey[0] = 3
	if document.LegacyBrowserSigningKey()[0] != 42 {
		t.Fatal("seed getter/clone exposes mutable state")
	}
	raw, _, err := encode(document)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := decode(raw)
	if err != nil || !bytes.Equal(loaded.LegacyBrowserSigningKey(), document.LegacyBrowserSigningKey()) {
		t.Fatal("seed lost in storage codec", err)
	}
	if document.WithLegacyBrowserSigningKey([]byte("invalid"), 1).Validate() == nil {
		t.Fatal("invalid cookie seed length accepted")
	}
	store, _ := fixtureStore(t)
	snapshot, err := store.Create(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	next := snapshot.Document.WithLegacyBrowserSigningKey(bytes.Repeat([]byte{43}, 32), 1)
	if _, err := store.Replace(context.Background(), snapshot.Version, next); !errors.Is(err, ErrInvalid) {
		t.Fatal("compatibility signing seed changed", err)
	}
	next = snapshot.Document.WithLegacyBrowserSigningKey(document.LegacyBrowserSigningKey(), 2)
	if _, err := store.Replace(context.Background(), snapshot.Version, next); !errors.Is(err, ErrInvalid) {
		t.Fatal("compatibility generation changed", err)
	}
	if loaded.Credentials[0].LegacyBrowserGeneration() != 1 {
		t.Fatal("credential compatibility generation lost in codec")
	}
	next = snapshot.Document.Clone()
	next.Credentials[0] = next.Credentials[0].WithLegacyBrowserGeneration(0)
	if _, err := store.Replace(context.Background(), snapshot.Version, next); !errors.Is(err, ErrInvalid) {
		t.Fatal("credential adoption binding changed", err)
	}
	next = snapshot.Document.Clone()
	newCredential := next.Credentials[1].WithLegacyBrowserGeneration(1).WithSecretMaterial([]byte("new-fixture-material"))
	newCredential.ID = "credential_new_fixture"
	newCredential.Name = "New integration"
	next.Credentials = append(next.Credentials, newCredential)
	next.Operations = append(next.Operations, fixtureOperation("create", newCredential.ID))
	if _, err := store.Replace(context.Background(), snapshot.Version, next); !errors.Is(err, ErrInvalid) {
		t.Fatal("new credential acquired a V1 adoption binding", err)
	}
	if loaded.LegacyBrowserSigningGeneration() != 1 {
		t.Fatal("compatibility generation lost in codec")
	}
	if document.WithLegacyBrowserSigningKey(nil, 1).Validate() == nil || document.WithLegacyBrowserSigningKey(document.LegacyBrowserSigningKey(), 0).Validate() == nil {
		t.Fatal("incomplete compatibility state accepted")
	}
	if _, err := json.Marshal(document); !errors.Is(err, ErrSensitive) {
		t.Fatal("public JSON exposes compatibility signing seed")
	}
	if strings.Contains(fmt.Sprintf("%+v", document), "legacyBrowserSigningKey") {
		t.Fatal("diagnostic exposes compatibility state")
	}
}
