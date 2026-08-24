package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/matty-v/kyber/pkg/skillscan"
	"github.com/matty-v/kyber/pkg/skillstore"
)

// SkillsResponse is the read-only view of one agent's skills.
//
// Read-only is the whole contract: skills are added, changed, and removed by
// talking to the agent, which writes them into its identity repo and pushes.
// There is deliberately no POST, PUT, or DELETE here — an operator editing a
// skill behind the agent's back would leave the repo and the pod disagreeing,
// which is the state this endpoint exists to make visible.
type SkillsResponse struct {
	// Agent is the agent these skills belong to.
	Agent string `json:"agent"`
	// ReportedAt is when the control plane accepted this report (RFC3339).
	// Agents report at boot and on every identity sync, so this can be old
	// on a long-running pod without anything being wrong.
	ReportedAt string `json:"reportedAt"`
	// Skills is what the agent found on its own filesystem.
	Skills []skillscan.Skill `json:"skills"`
	// Issues are problems that belong to no single skill.
	Issues []skillscan.Issue `json:"issues"`
	// Summary is the at-a-glance count the PWA renders as a badge.
	Summary SkillsSummary `json:"summary"`
}

// SkillsSummary counts the report so the caller does not have to.
//
// Broken and Warnings are kept apart deliberately. A skill no runtime can load
// and a skill missing its description are both findings, but only one of them
// means the agent cannot do the thing — collapsing them into a single count
// would bury the number an operator actually acts on.
type SkillsSummary struct {
	// Total is every skill found.
	Total int `json:"total"`
	// Broken carries at least one error-severity issue: it exists and does
	// not work.
	Broken int `json:"broken"`
	// Warnings works, but has something worth fixing.
	Warnings int `json:"warnings"`
	// Healthy has nothing reported against it at all.
	Healthy int `json:"healthy"`
	// OtherIssues counts report-level problems (stray or dangling state in
	// a runtime skills home).
	OtherIssues int `json:"otherIssues"`
}

// handleAgentSkills handles GET /api/v1/agents/{name}/skills.
//
// 404 means the agent has never reported — a pod that has not booted since the
// feature shipped, or an install with no identity repo. That is reported as its
// own state rather than an empty list, because "no skills" and "we have not
// heard from this agent" must not render the same.
func (s *Server) handleAgentSkills(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if s.SkillStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable",
			"skill reporting is not enabled on this install")
		return
	}
	rep, err := s.SkillStore.Get(r.Context(), name)
	if err != nil {
		if errors.Is(err, skillstore.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "not_found",
				"agent '"+name+"' has not reported its skills yet — it reports at boot and on every identity sync")
			return
		}
		writeJSONError(w, http.StatusServiceUnavailable, "store_error", "skill store error")
		return
	}

	resp := SkillsResponse{
		Agent:      name,
		ReportedAt: rep.ReportedAt,
		Skills:     rep.Skills,
		Issues:     rep.Issues,
	}
	// Encode empty rather than null: the PWA renders these directly and a
	// null list is an extra branch in every consumer.
	if resp.Skills == nil {
		resp.Skills = []skillscan.Skill{}
	}
	if resp.Issues == nil {
		resp.Issues = []skillscan.Issue{}
	}
	resp.Summary.Total = len(resp.Skills)
	for _, sk := range resp.Skills {
		switch {
		case sk.Broken():
			resp.Summary.Broken++
		case sk.Healthy():
			resp.Summary.Healthy++
		default:
			resp.Summary.Warnings++
		}
	}
	resp.Summary.OtherIssues = len(resp.Issues)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=10")
	_ = json.NewEncoder(w).Encode(&resp)
}
