package api

import (
	"errors"
	"slices"
	"strings"
)

// Identity-repo modes an agent can be created with. "none" is always
// supported: the agent boots from its runtime image and persists only to its
// own volume. Both managed modes depend on the Kyber GitHub App — template
// mode to scaffold the repo, existing mode to mint the repo-scoped token the
// agent clones and pushes with (there is no PAT fallback for either).
const (
	IdentityRepoModeNone     = "none"
	IdentityRepoModeTemplate = "template"
	IdentityRepoModeExisting = "existing"
)

// identityRepoCapability reports which identity-repo modes this control plane
// can actually serve. It is the single source for both GET /api/v1/config and
// create-time validation, so what the wizard offers and what the API accepts
// cannot drift apart.
//
// Both managed modes require the App client AND the configured owner: the
// owner is where template repos are created, and the scoped-token mint trusts
// the identity repo to live in that same account (see handleIdentityRepoToken).
func (s *Server) identityRepoCapability() ConfigIdentity {
	c := ConfigIdentity{
		SupportedModes: []string{IdentityRepoModeNone},
		RepoOwner:      s.IdentityRepoOwner,
	}
	switch {
	case s.GithubAppClient == nil:
		c.UnavailableReason = "The Kyber GitHub App is not configured on this control plane " +
			"(the kyber-github-app Secret is missing or invalid), so agents can only be created " +
			"without an identity repository."
	case s.IdentityRepoOwner == "":
		c.UnavailableReason = "No identity-repo owner is configured (Helm value identityRepo.defaultOwner), " +
			"so agents can only be created without an identity repository."
	default:
		c.ManagedReposAvailable = true
		c.SupportedModes = []string{IdentityRepoModeTemplate, IdentityRepoModeExisting, IdentityRepoModeNone}
	}
	return c
}

// validateIdentityRepoRequest rejects identity-repo requests this control plane
// cannot serve. It must run before any Secret, PVC or Agent is created, so an
// unusable request leaves nothing behind. Omitting both fields is always valid
// and means a repo-less, volume-only agent.
func (s *Server) validateIdentityRepoRequest(req agentIdentityRepoRequest) error {
	if req.Repo != "" && req.Template != "" {
		return errors.New("identityRepo.repo and identityRepo.template are mutually exclusive — " +
			"set repo to link an existing repository, or template to create a new one, not both")
	}
	mode := IdentityRepoModeNone
	switch {
	case req.Template != "":
		mode = IdentityRepoModeTemplate
	case req.Repo != "":
		mode = IdentityRepoModeExisting
	}
	c := s.identityRepoCapability()
	if !slices.Contains(c.SupportedModes, mode) {
		action := "create an identity repository from a template"
		if mode == IdentityRepoModeExisting {
			action = "link an existing identity repository"
		}
		return errors.New("cannot " + action + ": " + c.UnavailableReason +
			" Omit identityRepo, or configure the GitHub App and identityRepo.defaultOwner and retry.")
	}
	// A malformed slug is otherwise accepted and fails only in the controller:
	// a bad template parks the agent in Creating with no retry, and a bad repo
	// is echoed into the pod's KYBER_IDENTITY_REPO.
	switch mode {
	case IdentityRepoModeTemplate:
		if !validIdentityRepoSlug(req.Template) {
			return errors.New("identityRepo.template must be an owner/name GitHub repository slug")
		}
	case IdentityRepoModeExisting:
		if !validIdentityRepoSlug(req.Repo) {
			return errors.New("identityRepo.repo must be an owner/name GitHub repository slug")
		}
	}
	return nil
}

// validIdentityRepoSlug reports whether s is exactly "owner/name" with both
// segments valid GitHub names.
func validIdentityRepoSlug(s string) bool {
	owner, name, ok := strings.Cut(s, "/")
	return ok && githubOwnerRepoRe.MatchString(owner) && githubOwnerRepoRe.MatchString(name)
}
