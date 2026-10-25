package api

import (
	"crypto/subtle"
	"encoding/json"
	"github.com/matty-v/kyber/pkg/securityaudit"
	"net/http"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// These DTOs deliberately have no key, key fragment, digest, cookie, or Secret
// data fields. Inventory describes accepted authority, never credential values.
type operatorCredentialView struct {
	ActivityUnavailableReason string   `json:"activityUnavailableReason,omitempty"`
	PrincipalID               string   `json:"principalId,omitempty"`
	TenantID                  string   `json:"tenantId,omitempty"`
	ActivityTrackable         bool     `json:"activityTrackable"`
	CreatedAt                 string   `json:"createdAt,omitempty"`
	ExpiresAt                 string   `json:"expiresAt,omitempty"`
	ID                        string   `json:"id"`
	Name                      string   `json:"name"`
	DisplayName               string   `json:"displayName"`
	AccessLevel               string   `json:"accessLevel"`
	State                     string   `json:"state"`
	Source                    string   `json:"source"`
	Scopes                    []string `json:"scopes"`
	AgentResources            []string `json:"agentResources"`
	MachineResources          []string `json:"machineResources,omitempty"`
	Permissions               []string `json:"permissions"`
	Configured                bool     `json:"configured"`
	ConfiguredScopes          []string `json:"configuredScopes,omitempty"`
	ConfiguredResources       []string `json:"configuredResources,omitempty"`
	ConfiguredMachines        []string `json:"configuredMachines,omitempty"`
	ConfigurationDiffers      bool     `json:"configurationDiffers,omitempty"`
	AuthenticatesAs           string   `json:"authenticatesAs,omitempty"`
}

type operatorCredentialInventory struct {
	ConfigurationRotationAvailable bool                     `json:"configurationRotationAvailable"`
	AdoptionAvailable              bool                     `json:"adoptionAvailable,omitempty"`
	MaintenanceAvailable           bool                     `json:"maintenanceAvailable,omitempty"`
	CurrentCredentialID            string                   `json:"currentCredentialId,omitempty"`
	Version                        string                   `json:"version,omitempty"`
	ManagementAvailable            bool                     `json:"managementAvailable,omitempty"`
	ObservedAt                     string                   `json:"observedAt"`
	InventoryScope                 string                   `json:"inventoryScope"`
	ConfigurationAvailable         bool                     `json:"configurationAvailable"`
	UsageAvailable                 bool                     `json:"usageAvailable"`
	Credentials                    []operatorCredentialView `json:"credentials"`
	Warnings                       []string                 `json:"warnings"`
}

func credentialPermissions(scopes []string) []string {
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		switch Scope(scope) {
		case ScopeLifecycleWrite, ScopeLifecycleAdmin:
			out = append(out, "Start, stop, and restart allowed Agents", "Change their models, runtime versions, CPU, memory, and disk allocation", "Restart or compact their sessions", "Repair their runtimes")
			if Scope(scope) == ScopeLifecycleAdmin {
				out = append(out, "Delete allowed Agents or require reauthentication")
			}
		case ScopeTasksCreate:
			out = append(out, "Submit tasks")
		case ScopeTasksRead:
			out = append(out, "Read owned tasks")
		case ScopeTasksList:
			out = append(out, "List owned tasks")
		case ScopeTasksContinue:
			out = append(out, "Respond to task interactions")
		case ScopeTasksCancel:
			out = append(out, "Cancel owned tasks")
		case ScopeTaskResultsRead:
			out = append(out, "Read task results")
		case ScopeTaskEventsRead:
			out = append(out, "Read task events")
		case ScopeRequestsWrite:
			out = append(out, "Submit Agent requests and task actions")
		case ScopeRequestsRead:
			out = append(out, "Read Agent requests and owned task data")
		case ScopeCapabilitiesRead:
			out = append(out, "Read published Agent capabilities")
		case ScopeCapabilitiesWrite:
			out = append(out, "Publish or remove Agent capabilities")
		case ScopeUsageRead:
			out = append(out, "Read observed token usage")
		case ScopeAgentExec:
			out = append(out, "Open terminals and run commands on allowed Agents")
		case ScopeMachineExec:
			out = append(out, "Open host terminals and run commands on allowed Machines")
		case ScopeSecretsList:
			out = append(out, "List user credential names and metadata on allowed Agents")
		case ScopeSecretsRead:
			out = append(out, "Read user credential values on allowed Agents")
		case ScopeSecretsWrite:
			out = append(out, "Create, replace, or delete user credentials on allowed Agents")
		case ScopeLogsRead:
			out = append(out, "Read operational logs on allowed Agents")
		case ScopeTranscriptsRead:
			out = append(out, "Read conversation transcripts on allowed Agents")
		case ScopeMachineLogsRead:
			out = append(out, "Read host operational logs on allowed Machines")
		case ScopeMetricsRead:
			out = append(out, "Read fleet operational metrics")
		case ScopeArchivesAdmin:
			out = append(out, "Export and import allowed Agent disks, including local files and credentials")
		case ScopeTasksAdmin, ScopeTasksPlatformAdmin:
			out = append(out, "Reserved task permission; no additional access")
		default:
			out = append(out, "Unknown permission")
		}
	}
	unique := make([]string, 0, len(out))
	seen := make(map[string]bool, len(out))
	for _, permission := range out {
		if !seen[permission] {
			unique = append(unique, permission)
			seen[permission] = true
		}
	}
	return unique
}

func sortedResources(resources ResourceSet) []string {
	if resources.all {
		return []string{"*"}
	}
	values := make([]string, 0, len(resources.resources))
	for v := range resources.resources {
		values = append(values, v)
	}
	sort.Strings(values)
	return values
}

func (a *APIKeyAuthenticator) credentialInventory() []operatorCredentialView {
	a.mu.RLock()
	defer a.mu.RUnlock()
	views := make([]operatorCredentialView, 0, len(a.callers)+1)
	if a.key != "" {
		views = append(views, operatorCredentialView{
			PrincipalID: installationAdminPrincipalID, TenantID: installationTenantID, ActivityTrackable: true, ID: installationAdminCredentialID, Name: "installation-admin", DisplayName: "Installation administrator",
			AccessLevel: "administrator", State: "accepted", Source: "installation",
			Scopes: []string{}, AgentResources: []string{"*"}, MachineResources: []string{"*"}, Permissions: []string{"Full access to this Kyber installation"},
		})
	}
	for i, entry := range a.callers {
		declared := entry.caller
		c := declared
		state, authenticatesAs := "accepted", ""
		// Authentication chooses the final matching key, with the installation
		// credential taking precedence. Reflect that actual authority instead
		// of advertising different restrictions for copies of the same key.
		for j := i + 1; j < len(a.callers); j++ {
			if subtle.ConstantTimeCompare([]byte(entry.key), []byte(a.callers[j].key)) == 1 {
				c = a.callers[j].caller
				state, authenticatesAs = "shared-key", c.Name
			}
		}
		if a.key != "" && subtle.ConstantTimeCompare([]byte(entry.key), []byte(a.key)) == 1 {
			c = *installationAdminCaller()
			state, authenticatesAs = "shared-key", "Installation administrator"
		}
		scopes := make([]string, 0, len(c.Scopes.scopes))
		for scope := range c.Scopes.scopes {
			scopes = append(scopes, string(scope))
		}
		sort.Strings(scopes)
		id := declared.CredentialID
		if id == "" {
			id = "configured-" + strconv.Itoa(i+1)
		}
		view := operatorCredentialView{
			PrincipalID: declared.PrincipalID, TenantID: declared.TenantID, ActivityTrackable: state == "accepted" && declared.CredentialGeneration > 0 && (securityaudit.CredentialUsageIdentity{CredentialID: declared.CredentialID, PrincipalID: declared.PrincipalID, TenantID: declared.TenantID, Authority: "configuration"}).Validate() == nil, ID: id, Name: declared.Name, DisplayName: declared.Name, AccessLevel: "restricted", State: state, Source: "configuration",
			Scopes: scopes, AgentResources: sortedResources(c.AgentResources), MachineResources: sortedResources(c.MachineResources), Permissions: credentialPermissions(scopes),
			AuthenticatesAs: authenticatesAs,
		}
		if c.Scopes.full {
			view.AccessLevel = "administrator"
			view.Permissions = []string{"Full access to this Kyber installation"}
		}
		if entry.key == "" {
			view.State, view.AuthenticatesAs = "configured-not-accepted", ""
			view.AccessLevel = "restricted"
			view.Scopes, view.AgentResources, view.MachineResources, view.Permissions = []string{}, []string{}, []string{}, []string{}
		}
		views = append(views, view)
	}
	counts := map[string]int{}
	for _, view := range views {
		if view.ActivityTrackable {
			counts[view.ID+"\x00"+view.PrincipalID+"\x00"+view.TenantID]++
		}
	}
	for i := range views {
		view := &views[i]
		if view.State == "shared-key" {
			view.ActivityUnavailableReason = "shared-key"
			continue
		}
		if !view.ActivityTrackable {
			view.ActivityUnavailableReason = "identity-incomplete"
			continue
		}
		if counts[view.ID+"\x00"+view.PrincipalID+"\x00"+view.TenantID] > 1 {
			view.ActivityTrackable = false
			view.ActivityUnavailableReason = "identity-shared"
		}
	}
	return views
}

func (s *Server) handleOperatorCredentials(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	if !s.requireInstallationAdmin(w, r, "list-operator-credentials") {
		return
	}
	resolved, err := s.auth.resolve(r.Context())
	if err != nil {
		writeOperatorAuthenticationUnavailable(w)
		return
	}
	if resolved.registry != nil {
		actor := callerFrom(r.Context())
		entry, ok := resolved.registry.byID[actor.CredentialID]
		if !ok || !entry.caller.Scopes.full || !sameCredentialIdentity(*actor, entry.caller) {
			writeJSONError(w, http.StatusForbidden, "forbidden", "Current administrator access required")
			return
		}
		response := operatorCredentialInventory{CurrentCredentialID: callerFrom(r.Context()).CredentialID, ObservedAt: time.Now().UTC().Format(time.RFC3339), InventoryScope: "installation-registry", Credentials: resolved.registry.inventory, Warnings: []string{"Usage totals are not available."}}
		_, _, supported := s.credentialManagementDependencies()
		if resolved.registry.version != "" && resolved.registry.uid != "" {
			response.Version = credentialRegistryETag(resolved.registry.uid, resolved.registry.version)
			w.Header().Set("ETag", response.Version)
		}
		response.MaintenanceAvailable = s.credentialMaintenanceAvailable()
		_, response.AdoptionAvailable = s.adoptionManager()
		response.UsageAvailable = s.credentialUsageAvailable()
		if response.UsageAvailable {
			response.Warnings = []string{}
		}
		response.ManagementAvailable = supported && resolved.registry.uid != "" && response.Version != "" && !resolved.registry.adoptionPending
		writeJSON(w, http.StatusOK, response)
		return
	}
	response := operatorCredentialInventory{
		ConfigurationRotationAvailable: s.OperatorCredentialAuthority == nil,
		AdoptionAvailable:              s.OperatorCredentialAdoption != nil && s.OperatorCredentialAdoption.Namespace() == s.Namespace,
		CurrentCredentialID:            callerFrom(r.Context()).CredentialID,
		ObservedAt:                     time.Now().UTC().Format(time.RFC3339), InventoryScope: "current-control-plane",
		MaintenanceAvailable: s.credentialMaintenanceAvailable(), UsageAvailable: s.credentialUsageAvailable(), Credentials: s.auth.credentialInventory(), Warnings: []string{},
	}
	reader := s.APIReader
	if reader == nil {
		reader = s.K8sClient
	}
	if reader == nil || s.APIKeySecretName == "" {
		response.Warnings = append(response.Warnings, "Additional configured credentials could not be checked.")
		writeJSON(w, http.StatusOK, response)
		return
	}
	secret := &corev1.Secret{}
	if err := reader.Get(r.Context(), types.NamespacedName{Namespace: s.Namespace, Name: s.APIKeySecretName}, secret); err != nil {
		response.Warnings = append(response.Warnings, "Additional configured credentials could not be checked.")
		writeJSON(w, http.StatusOK, response)
		return
	}
	// Parse only safe metadata. Even malformed configuration must never appear
	// in an error message: the original document can contain inline key values.
	var configured []struct {
		Name             string   `json:"name"`
		CredentialID     string   `json:"credentialId"`
		Scopes           []string `json:"scopes"`
		AgentResources   []string `json:"agentResources"`
		MachineResources []string `json:"machineResources"`
	}
	if raw := secret.Data["callers"]; len(raw) > 0 && json.Unmarshal(raw, &configured) != nil {
		response.Warnings = append(response.Warnings, "The configured credential list could not be read. Active credentials are shown.")
		writeJSON(w, http.StatusOK, response)
		return
	}
	response.ConfigurationAvailable = true
	for i := range response.Credentials {
		if response.Credentials[i].Source == "installation" {
			response.Credentials[i].Configured = len(secret.Data[apiKeySecretField]) > 0
		}
	}
	for i, config := range configured {
		sort.Strings(config.Scopes)
		sort.Strings(config.AgentResources)
		sort.Strings(config.MachineResources)
		matched := false
		for j := range response.Credentials {
			view := &response.Credentials[j]
			if view.Source == "configuration" && view.Name == config.Name && (config.CredentialID == "" || config.CredentialID == view.ID) {
				view.Configured = true
				view.ConfiguredScopes = config.Scopes
				view.ConfiguredResources = config.AgentResources
				view.ConfiguredMachines = config.MachineResources
				view.ConfigurationDiffers = !equalStringSet(view.Scopes, config.Scopes) || !equalStringSet(view.AgentResources, sortedResources(newResourceSet(config.AgentResources...))) || !equalStringSet(view.MachineResources, sortedResources(newResourceSet(config.MachineResources...)))
				matched = true
			}
		}
		if !matched {
			id := config.CredentialID
			if id == "" {
				id = "inactive-configured-" + strconv.Itoa(i+1)
			}
			response.Credentials = append(response.Credentials, operatorCredentialView{
				ID: id, Name: config.Name, DisplayName: config.Name, AccessLevel: "restricted", State: "configured-not-accepted", Source: "configuration",
				Scopes: []string{}, AgentResources: []string{}, Permissions: []string{}, Configured: true,
				ConfiguredScopes: config.Scopes, ConfiguredResources: config.AgentResources, ConfiguredMachines: config.MachineResources,
			})
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func equalStringSet(a, b []string) bool {
	left, right := make(map[string]bool, len(a)), make(map[string]bool, len(b))
	for _, value := range a {
		left[value] = true
	}
	for _, value := range b {
		right[value] = true
	}
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if !right[value] {
			return false
		}
	}
	return true
}
