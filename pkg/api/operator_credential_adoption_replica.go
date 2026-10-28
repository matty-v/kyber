package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
)

var errCredentialAdoptionReplica = errors.New("loaded credential access could not be verified for adoption")

// A private, read-only gate for an already discovered, UID-pinned replica.
// Discovery, release/image ownership, all serving/terminating replicas, old
// writers and source ownership must still be verified by the supervised worker.
// Never substitute a Service address: it can verify the same replica repeatedly.
type credentialAdoptionReplicaProbe struct{ http *http.Client }

func newCredentialAdoptionReplicaProbe() *credentialAdoptionReplicaProbe {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, DisableKeepAlives: true, ResponseHeaderTimeout: 2 * time.Second}
	return &credentialAdoptionReplicaProbe{http: &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (p *credentialAdoptionReplicaProbe) verify(ctx context.Context, source *credentialAdoptionSources, pod corev1.Pod, port int) error {
	if p == nil || p.http == nil || source == nil || pod.Namespace != source.namespace || port < 1 || port > 65535 || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return errCredentialAdoptionReplica
	}
	id, err := uuid.Parse(string(pod.UID))
	if err != nil || id == uuid.Nil || id.String() != string(pod.UID) {
		return errCredentialAdoptionReplica
	}
	ip := net.ParseIP(pod.Status.PodIP)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return errCredentialAdoptionReplica
	}
	ready := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		return errCredentialAdoptionReplica
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if source.Verify(ctx) != nil {
		return errCredentialAdoptionReplica
	}
	base := "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(port))
	expected := NewAPIKeyAuthenticator(source.root, source.callers...)
	// Probe each distinct material using normal root/final-definition precedence.
	// Equal inventory IDs do not prove equal material-to-identity mappings.
	values := []string{source.root}
	seen := map[string]bool{source.root: true}
	for _, c := range source.callers {
		if !seen[c.Key] {
			values = append(values, c.Key)
			seen[c.Key] = true
		}
	}
	for _, value := range values {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/operator-credentials/current", nil)
		if err != nil {
			return errCredentialAdoptionReplica
		}
		request.Header.Set("Authorization", "Bearer "+value)
		caller, err := expected.authenticateConfigured(request)
		if err != nil || caller == nil {
			return errCredentialAdoptionReplica
		}
		var observed operatorCredentialCurrentView
		if p.read(request, &observed) != nil {
			return errCredentialAdoptionReplica
		}
		if _, err := time.Parse(time.RFC3339Nano, observed.ObservedAt); err != nil {
			return errCredentialAdoptionReplica
		}
		observed.ObservedAt = ""
		want := currentOperatorCredentialView(*caller, source.namespace, string(pod.UID), time.Now())
		want.ObservedAt = ""
		if !reflect.DeepEqual(observed, want) {
			return errCredentialAdoptionReplica
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/operator-credentials", nil)
	if err != nil {
		return errCredentialAdoptionReplica
	}
	request.Header.Set("Authorization", "Bearer "+source.root)
	var inventory operatorCredentialInventory
	if p.read(request, &inventory) != nil || inventory.InventoryScope != "current-control-plane" || inventory.ManagementAvailable {
		return errCredentialAdoptionReplica
	}
	// Ignore usage/configuration observations, but require every accepted or
	// shadowed definition and its declared identity/effective grants to match.
	want := expected.credentialInventory()
	if len(inventory.Credentials) != len(want) {
		return errCredentialAdoptionReplica
	}
	for i, c := range inventory.Credentials {
		e := want[i]
		if c.ID != e.ID || c.PrincipalID != e.PrincipalID || c.TenantID != e.TenantID || c.Name != e.Name || c.DisplayName != e.DisplayName || c.State != e.State || c.Source != e.Source || c.AccessLevel != e.AccessLevel || c.AuthenticatesAs != e.AuthenticatesAs || !slices.Equal(c.Scopes, e.Scopes) || !slices.Equal(c.AgentResources, e.AgentResources) || !slices.Equal(c.MachineResources, e.MachineResources) || !slices.Equal(c.Permissions, e.Permissions) {
			return errCredentialAdoptionReplica
		}
	}
	if ctx.Err() != nil || source.Verify(ctx) != nil {
		return errCredentialAdoptionReplica
	}
	return nil
}

func (p *credentialAdoptionReplicaProbe) read(request *http.Request, target any) error {
	response, err := p.http.Do(request)
	if err != nil {
		return errCredentialAdoptionReplica
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		return errCredentialAdoptionReplica
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return errCredentialAdoptionReplica
	}
	if json.Unmarshal(data, target) != nil {
		return errCredentialAdoptionReplica
	}
	return nil
}
