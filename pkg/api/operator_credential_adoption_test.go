package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/matty-v/kyber/pkg/operatorcredentials"
)

func adoptionFixtureOperation() operatorcredentials.Operation {
	return operatorcredentials.Operation{ID: uuid.NewString(), AdmissionEventID: uuid.NewString(), CompletionEventID: uuid.NewString(), RequestID: uuid.NewString(), Action: "adopt", ActorPrincipalID: installationAdminPrincipalID, ActorCredentialID: installationAdminCredentialID, ActorGeneration: 1, Authority: "configuration", At: time.Now().UTC(), Status: "pending"}
}

func adoptionFixtureCaller() ScopedCaller {
	return ScopedCaller{Name: "Task integration", Key: "private-adoption-client-value", PrincipalID: "principal-existing", TenantID: "tenant-existing", CredentialID: "credential-existing", CredentialGeneration: 9007199254740993, Scopes: []string{string(ScopeTasksRead), string(ScopeTasksCreate)}, AgentResources: []string{"kyber-system/alpha"}, Kind: "integration", Issuer: "operator", Subject: "existing-consumer"}
}

func adoptionBearer(key string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

func TestCredentialAdoptionPreservesMaterialIdentityGrantsAndCookies(t *testing.T) {
	const root = "private-adoption-installation-value"
	configured := adoptionFixtureCaller()
	auth := NewAPIKeyAuthenticator(root, configured)
	before, err := auth.authenticateConfigured(adoptionBearer(configured.Key))
	if err != nil {
		t.Fatal(err)
	}
	rootCookie, err := auth.CreateBrowserSession(*installationAdminCaller())
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := auth.CreateBrowserSession(*before)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := auth.prepareCredentialAdoption(context.Background(), "kyber-system", adoptionFixtureOperation())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := compileRegistryCredentials(candidate.document, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	after, err := registry.authenticate(adoptionBearer(configured.Key))
	if err != nil {
		t.Fatal(err)
	}
	if !sameCredentialIdentity(*before, *after) || before.Kind != after.Kind || before.Issuer != after.Issuer || before.Subject != after.Subject || !reflect.DeepEqual(before.Scopes, after.Scopes) || !reflect.DeepEqual(before.AgentResources, after.AgentResources) {
		t.Fatal("adoption changed existing authority")
	}
	for _, token := range []string{rootCookie, cookie} {
		if _, _, err := registry.browserCaller(token, time.Now()); err != nil {
			t.Fatal("current cookie invalidated", err)
		}
	}
	if candidate.identityAssignments != 0 || candidate.browserReconnects != 0 || candidate.shadowedDefinitions != 0 {
		t.Fatal("invented migration observations")
	}
	for _, credential := range candidate.document.Credentials {
		if credential.CreatedAt != nil {
			t.Fatal("imported issuance time invented")
		}
	}
	if current, err := auth.authenticateConfigured(adoptionBearer(configured.Key)); err != nil || !sameCredentialIdentity(*current, *before) {
		t.Fatal("candidate preparation changed live auth")
	}
	if _, err := json.Marshal(candidate); !errors.Is(err, operatorcredentials.ErrSensitive) {
		t.Fatal("candidate serialized")
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%q", "%x"} {
		value := fmt.Sprintf(format, *candidate)
		if strings.Contains(value, root) || strings.Contains(value, configured.Key) || !strings.Contains(value, "[redacted]") {
			t.Fatal("candidate diagnostics exposed material")
		}
	}
}

func TestCredentialAdoptionKeepsActualDuplicateKeyPrecedence(t *testing.T) {
	first := adoptionFixtureCaller()
	first.Scopes = []string{string(ScopeTasksCreate)}
	winner := first
	winner.Name = "Final integration"
	winner.PrincipalID = "principal-final"
	winner.CredentialID = "credential-final"
	winner.Scopes = []string{string(ScopeTasksRead)}
	rootCopy := first
	rootCopy.Name = "Root copy"
	rootCopy.Key = "private-adoption-root-value"
	a := NewAPIKeyAuthenticator(rootCopy.Key, first, winner, rootCopy)
	candidate, err := a.prepareCredentialAdoption(context.Background(), "kyber-system", adoptionFixtureOperation())
	if err != nil {
		t.Fatal(err)
	}
	if candidate.shadowedDefinitions != 2 || len(candidate.document.Credentials) != 2 {
		t.Fatal("duplicate materials were not collapsed")
	}
	registry, err := compileRegistryCredentials(candidate.document, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{first.Key, rootCopy.Key} {
		before, err := a.authenticateConfigured(adoptionBearer(key))
		if err != nil {
			t.Fatal(err)
		}
		after, err := registry.authenticate(adoptionBearer(key))
		if err != nil {
			t.Fatal(err)
		}
		if !sameCredentialIdentity(*before, *after) || !reflect.DeepEqual(before.Scopes, after.Scopes) {
			t.Fatal("duplicate grants were merged or precedence changed")
		}
	}
}

func TestCredentialAdoptionAssignsOnlyMissingIdentityWithoutBorrowingOwnership(t *testing.T) {
	partial := adoptionFixtureCaller()
	partial.PrincipalID = installationAdminPrincipalID
	partial.TenantID = ""
	partial.CredentialGeneration = 1
	preIdentity := ScopedCaller{Name: "legacy", Key: "private-pre-identity-value", Scopes: []string{string(ScopeMetricsRead)}}
	a := NewAPIKeyAuthenticator("private-adoption-root-value", partial, preIdentity)
	old, err := a.authenticateConfigured(adoptionBearer(preIdentity.Key))
	if err != nil {
		t.Fatal(err)
	}
	oldCookie, err := a.CreateBrowserSession(*old)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := a.prepareCredentialAdoption(context.Background(), "kyber-system", adoptionFixtureOperation())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := compileRegistryCredentials(candidate.document, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	current, err := registry.authenticate(adoptionBearer(partial.Key))
	if err != nil {
		t.Fatal(err)
	}
	if current.PrincipalID != partial.PrincipalID || current.TenantID == "" || current.TenantID == installationTenantID {
		t.Fatal("missing tenant borrowed another principal's ownership")
	}
	if candidate.identityAssignments != 2 {
		t.Fatal("missing identity accounting incorrect")
	}
	imported, _, err := registry.browserCaller(oldCookie, time.Now())
	if err != nil {
		t.Fatal("pre-identity cookie not preserved", err)
	}
	if imported.PrincipalID == "" || imported.CredentialID == "" || strings.EqualFold(imported.Name, "legacy") {
		t.Fatal("imported identity remains unsupported")
	}
}

func TestCredentialAdoptionRejectsAmbiguousIdentityAndUnavailableAuthority(t *testing.T) {
	one := adoptionFixtureCaller()
	two := one
	two.Name = "Another integration"
	two.Key = "private-distinct-second-value"
	a := NewAPIKeyAuthenticator("private-adoption-root-value", one, two)
	if _, err := a.prepareCredentialAdoption(context.Background(), "kyber-system", adoptionFixtureOperation()); !errors.Is(err, errCredentialAdoptionCandidate) {
		t.Fatal("duplicate credential ID silently rewritten", err)
	}
	a = NewAPIKeyAuthenticator("private-adoption-root-value", one)
	a.authority = &memoryOperatorAuthority{err: operatorcredentials.ErrUnavailable}
	if _, err := a.prepareCredentialAdoption(context.Background(), "kyber-system", adoptionFixtureOperation()); !errors.Is(err, errOperatorAuthorityUnavailable) {
		t.Fatal("authority outage allowed configuration adoption", err)
	}
	a.authority = &memoryOperatorAuthority{document: registryFixtureDocument(t)}
	a.namespace = "kyber-system"
	if _, err := a.prepareCredentialAdoption(context.Background(), "kyber-system", adoptionFixtureOperation()); !errors.Is(err, errCredentialAdoptionCandidate) {
		t.Fatal("selected authority allowed re-adoption", err)
	}
}
