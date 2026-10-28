package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/matty-v/kyber/pkg/archivejob"
)

type archiveDownloadClaims struct {
	Version    int    `json:"v"`
	Job        string `json:"job"`
	Agent      string `json:"agent"`
	Namespace  string `json:"namespace"`
	Expires    int64  `json:"expires"`
	Principal  string `json:"principal"`
	Tenant     string `json:"tenant"`
	Credential string `json:"credential"`
	Generation uint64 `json:"generation"`
	Authority  string `json:"authority"`
	Name       string `json:"name,omitempty"`
	Binding    string `json:"binding"`
}

var errArchiveDownloadAccess = errors.New("archive download access unavailable")

func archiveCallerAuthority(c *Caller) string {
	if c.CredentialAuthority == "registry" {
		return "registry"
	}
	if c.Scopes.full && sameCredentialIdentity(*c, *installationAdminCaller()) {
		return "configuration-installation"
	}
	return "configuration-caller"
}

func (a *ArchiveService) downloadSignature(domain, value string) []byte {
	mac := hmac.New(sha256.New, a.SigningKey)
	_, _ = mac.Write([]byte(domain + "\x00" + value))
	return mac.Sum(nil)
}

func (a *ArchiveService) downloadMaterialBinding(key string) string {
	return base64.RawURLEncoding.EncodeToString(a.downloadSignature("kyber archive credential material v2", key))
}

func (s *Server) signCurrentArchiveDownload(r *http.Request, j *archivejob.Job, exp time.Time) (string, error) {
	caller := callerFrom(r.Context())
	if caller == nil || s.auth == nil {
		return "", errArchiveDownloadAccess
	}
	key, err := s.auth.credentialKey(r.Context(), caller)
	if err != nil {
		return "", err
	}
	c := archiveDownloadClaims{Version: 2, Job: j.ID, Agent: j.Agent, Namespace: s.Namespace, Expires: exp.Unix(), Principal: caller.PrincipalID, Tenant: caller.TenantID, Credential: caller.CredentialID, Generation: caller.CredentialGeneration, Authority: archiveCallerAuthority(caller), Binding: s.Archives.downloadMaterialBinding(key)}
	// Old configured callers may lack stable IDs. Their name is an additional
	// discriminator, never an authority or role. Managed credentials use IDs only.
	if c.Principal == "" || c.Tenant == "" || c.Credential == "" {
		c.Name = caller.Name
	}
	// Re-resolve with normal bearer precedence before publishing the capability.
	if _, err := s.resolveArchiveDownload(r.Context(), c); err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", errArchiveDownloadAccess
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return "v2." + body + "." + base64.RawURLEncoding.EncodeToString(s.Archives.downloadSignature("kyber archive download v2", body)), nil
}

func (s *Server) parseCurrentArchiveDownload(token string) (archiveDownloadClaims, bool) {
	var c archiveDownloadClaims
	if len(token) > 8192 {
		return c, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v2" {
		return c, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, s.Archives.downloadSignature("kyber archive download v2", parts[1])) {
		return c, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(raw, &c) != nil || c.Version != 2 || c.Job == "" || c.Agent == "" || c.Namespace != s.Namespace || c.Binding == "" || !s.Archives.now().Before(time.Unix(c.Expires, 0)) {
		return archiveDownloadClaims{}, false
	}
	return c, true
}

func (s *Server) resolveArchiveDownload(ctx context.Context, claims archiveDownloadClaims) (*Caller, error) {
	if s.auth == nil {
		return nil, errOperatorAuthorityUnavailable
	}
	resolved, err := s.auth.resolve(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []scopedKey
	if resolved.registry != nil {
		if claims.Authority != "registry" {
			return nil, errArchiveDownloadAccess
		}
		candidates = resolved.registry.entries
	} else {
		if claims.Authority == "registry" {
			return nil, errArchiveDownloadAccess
		}
		resolved.mu.RLock()
		candidates = append(candidates, resolved.callers...)
		candidates = append(candidates, scopedKey{key: resolved.key, caller: *installationAdminCaller()})
		resolved.mu.RUnlock()
	}
	for _, candidate := range candidates {
		if candidate.key == "" || !hmac.Equal([]byte(claims.Binding), []byte(s.Archives.downloadMaterialBinding(candidate.key))) {
			continue
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://archive-capability.invalid/", nil)
		request.Header.Set("Authorization", "Bearer "+candidate.key)
		var current *Caller
		if resolved.registry != nil {
			current, err = resolved.registry.authenticate(request)
		} else {
			current, err = resolved.authenticateConfigured(request)
		}
		if err != nil || current == nil {
			continue
		}
		expected := Caller{PrincipalID: claims.Principal, TenantID: claims.Tenant, CredentialID: claims.Credential, CredentialGeneration: claims.Generation}
		if !sameCredentialIdentity(*current, expected) || archiveCallerAuthority(current) != claims.Authority || claims.Name != "" && current.Name != claims.Name {
			continue
		}
		if !current.Scopes.Has(ScopeArchivesAdmin) || !current.AgentResources.Has(claims.Namespace+"/"+claims.Agent) {
			return nil, errArchiveDownloadAccess
		}
		return current, nil
	}
	return nil, errArchiveDownloadAccess
}
