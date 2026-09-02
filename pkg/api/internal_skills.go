package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kyberv1 "github.com/matty-v/kyber/pkg/api/v1"
	"github.com/matty-v/kyber/pkg/skillscan"
)

// Bounds on an accepted skill report. The agent scans its own filesystem, so
// the body is only as trustworthy as the pod: a runaway or malformed scan must
// cost the control plane a 400, not a fleet-wide memory or storage problem.
const (
	maxSkillReportBytes = 512 << 10
	maxReportedSkills   = 200
	maxIssuesPerSkill   = 20
	maxReportIssues     = 200
	maxSkillNameLen     = 128
	maxSkillDescLen     = 1024
	maxSkillPathLen     = 512
	maxIssueCodeLen     = 64
	maxIssueDetailLen   = 1024
	maxLinkedPerSkill   = 8
	maxSourcePackageLen = 128
)

// validSeverities is the closed set of issue severities.
var validSeverities = map[string]bool{
	skillscan.SeverityError:   true,
	skillscan.SeverityWarning: true,
}

// validSkillSources is the closed set of origins a reported skill may claim.
var validSkillSources = map[string]bool{
	skillscan.SourceIdentity: true,
	skillscan.SourceVendor:   true,
	skillscan.SourcePlatform: true,
}

// validLinkedRuntimes is the closed set of runtime identifiers a skill may
// claim to be loadable in. Anything else is a reporter bug or a forged body.
var validLinkedRuntimes = map[string]bool{
	skillscan.RuntimeClaudeCode: true,
	skillscan.RuntimeCodex:      true,
}

// handleSkillsReport handles POST /internal/agents/{name}/skills.
//
// The body is the report `kyber-skills report` produced by scanning the pod's
// own identity repo and runtime skill homes, forwarded by the status sidecar
// (which supplies agent identity and auth — both already verified by
// handleAgentRoutes before this runs, so agentName is the caller's own name and
// one agent can never write another's skills).
//
// ReportedAt is stamped here rather than trusted from the body: it is what the
// PWA renders as "as of", and a skewed pod clock would otherwise make a fresh
// report look stale, or a stale one look current.
func (s *InternalServer) handleSkillsReport(w http.ResponseWriter, r *http.Request, agentName string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.skillStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "skill_store_unavailable",
			"skill reporting is not enabled on this install")
		return
	}

	var report skillscan.Report
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSkillReportBytes))
	if err := dec.Decode(&report); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if report.Version != skillscan.ReportVersion {
		http.Error(w, "unsupported report version", http.StatusBadRequest)
		return
	}
	if err := sanitizeSkillReport(&report); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	report.ReportedAt = time.Now().UTC().Format(time.RFC3339Nano)

	if err := s.skillStore.Put(r.Context(), agentName, &report); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "skill_store_error",
			"skill report storage is unavailable")
		return
	}
	// Skill reports are durable-store writes, not Kubernetes objects, so they
	// would not otherwise enqueue the Agent reconciler. Touch a private
	// metadata annotation to make availability converge immediately. Failure is
	// best-effort: the report remains accepted and the next natural reconcile
	// still joins it.
	s.triggerCapabilityReconcile(r.Context(), agentName, report.ReportedAt)
	w.WriteHeader(http.StatusNoContent)
}

func (s *InternalServer) triggerCapabilityReconcile(ctx context.Context, agentName, reportedAt string) {
	if s.k8sClient == nil || s.namespace == "" {
		return
	}
	agent := &kyberv1.Agent{}
	if err := s.k8sClient.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: agentName}, agent); err != nil {
		return
	}
	if agent.Spec.PublicCapabilities == nil {
		return
	}
	patch := client.MergeFrom(agent.DeepCopy())
	if agent.Annotations == nil {
		agent.Annotations = map[string]string{}
	}
	agent.Annotations["kyber.io/capability-evidence-reported-at"] = reportedAt
	_ = s.k8sClient.Patch(ctx, agent, patch)
}

// sanitizeSkillReport enforces the wire bounds and normalizes whitespace in
// place. It rejects rather than truncates: a silently shortened report would
// show the operator a skill list that does not match the pod, which is the
// exact class of lie this feature exists to remove.
func sanitizeSkillReport(rep *skillscan.Report) error {
	if len(rep.Skills) > maxReportedSkills {
		return errTooMany("skills", maxReportedSkills)
	}
	if len(rep.Issues) > maxReportIssues {
		return errTooMany("report issues", maxReportIssues)
	}
	if err := sanitizeIssues(rep.Issues); err != nil {
		return err
	}
	for i := range rep.Skills {
		sk := &rep.Skills[i]
		sk.Name = strings.TrimSpace(sk.Name)
		sk.Description = strings.TrimSpace(sk.Description)
		sk.Path = strings.TrimSpace(sk.Path)
		sk.SourcePackage = strings.TrimSpace(sk.SourcePackage)

		if sk.Name == "" || len(sk.Name) > maxSkillNameLen {
			return errBadField("skill name")
		}
		if len(sk.Description) > maxSkillDescLen {
			return errBadField("skill description")
		}
		if len(sk.Path) > maxSkillPathLen {
			return errBadField("skill path")
		}
		if len(sk.SourcePackage) > maxSourcePackageLen {
			return errBadField("skill sourcePackage")
		}
		if !validSkillSources[sk.Source] {
			return errBadField("skill source")
		}
		if len(sk.Linked) > maxLinkedPerSkill {
			return errTooMany("linked runtimes", maxLinkedPerSkill)
		}
		for _, rt := range sk.Linked {
			if !validLinkedRuntimes[rt] {
				return errBadField("linked runtime")
			}
		}
		if len(sk.Issues) > maxIssuesPerSkill {
			return errTooMany("skill issues", maxIssuesPerSkill)
		}
		if err := sanitizeIssues(sk.Issues); err != nil {
			return err
		}
	}
	return nil
}

func sanitizeIssues(issues []skillscan.Issue) error {
	for i := range issues {
		issues[i].Code = strings.TrimSpace(issues[i].Code)
		issues[i].Detail = strings.TrimSpace(issues[i].Detail)
		issues[i].Severity = strings.TrimSpace(issues[i].Severity)
		if issues[i].Code == "" || len(issues[i].Code) > maxIssueCodeLen {
			return errBadField("issue code")
		}
		if len(issues[i].Detail) > maxIssueDetailLen {
			return errBadField("issue detail")
		}
		// Runtime images are pinned per cluster, so a control plane routinely
		// runs ahead of the agent images reporting into it. An older reporter
		// sends no severity; treat that as an error rather than rejecting the
		// report, so an image lag degrades the nuance instead of blanking the
		// tab entirely.
		if issues[i].Severity == "" {
			issues[i].Severity = skillscan.SeverityError
		}
		if !validSeverities[issues[i].Severity] {
			return errBadField("issue severity")
		}
	}
	return nil
}

type skillReportError string

func (e skillReportError) Error() string { return string(e) }

func errTooMany(what string, limit int) error {
	return skillReportError("too many " + what + " (limit " + strconv.Itoa(limit) + ")")
}

func errBadField(what string) error {
	return skillReportError("invalid " + what)
}
