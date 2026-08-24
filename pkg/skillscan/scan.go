// Package skillscan discovers the skills an agent actually has and reports
// whether each one is really loadable by the runtime.
//
// Every Kyber agent keeps its skills in one predictable place — the identity
// repo, as skills/<name>/SKILL.md — and the boot/sync linker
// (images/shared/kyber-identity-repo.sh) symlinks each package into BOTH
// runtime homes, ~/.claude/skills and ~/.codex/skills. That layout is the
// contract this package verifies.
//
// Two other sources of skills exist and are reported too: packages vendored
// into the identity repo under vendor/<pkg>/skills, and the capability
// cookbooks Kyber bakes into the runtime image and links in only when the
// matching sidecar is present (the Telegram and Discord skills). All three are
// skills the agent genuinely has, so all three show up.
//
// It deliberately scans the pod, not GitHub. During kyber#691 every identity
// repo on the fleet held a full set of skills and not one of them was linked
// into a path the runtime read; a repo-sourced view would have shown a healthy
// list for the entire outage. What an agent can actually invoke is a fact about
// its filesystem, so that is what gets measured here.
package skillscan

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ReportVersion is the wire-schema version of Report. Bump it when the shape
// changes incompatibly; the control plane rejects versions it does not know.
const ReportVersion = 1

// Source values for Skill.Source.
const (
	// SourceIdentity is a skill the agent owns, under <repo>/skills/.
	SourceIdentity = "identity"
	// SourceVendor is a skill vendored from a shared package, under
	// <repo>/vendor/<package>/skills/.
	SourceVendor = "vendor"
	// SourcePlatform is a capability cookbook baked into the runtime image
	// and linked in by the start script when the matching sidecar exists
	// (see the Telegram/Discord blocks in images/*/start-*.sh). It is not in
	// the identity repo and is not the agent's to edit.
	SourcePlatform = "platform"
)

// DefaultPlatformDir is where the runtime images install their bundled skills.
// Overridable in the images via KYBER_PLATFORM_SKILLS_DIR.
const DefaultPlatformDir = "/opt/kyber/skills"

// Runtime identifiers used in Skill.Linked and in NotLinked issue details.
// These are the two runtime homes the linker maintains.
const (
	RuntimeClaudeCode = "claude-code"
	RuntimeCodex      = "codex"
)

// runtimeHomes maps a runtime identifier to the skills directory, relative to
// $HOME, that the runtime loads skills from.
var runtimeHomes = []struct {
	runtime string
	dir     string
}{
	{RuntimeClaudeCode, filepath.Join(".claude", "skills")},
	{RuntimeCodex, filepath.Join(".codex", "skills")},
}

// Issue codes. Each names a concrete way a skill fails to be loadable, or a
// way the on-disk state will not survive the next reprovision.
const (
	// IssueMissingSkillMD is a directory under skills/ with no SKILL.md, so
	// the linker skips it entirely and the skill silently does not exist.
	IssueMissingSkillMD = "missing_skill_md"
	// IssueInvalidFrontmatter is a SKILL.md whose YAML frontmatter is absent
	// or unparseable. Both runtimes need it to register the skill.
	IssueInvalidFrontmatter = "invalid_frontmatter"
	// IssueMissingDescription is frontmatter with no description. The skill
	// loads but never triggers implicitly, because there is nothing to match.
	IssueMissingDescription = "missing_description"
	// IssueNameMismatch is frontmatter whose name disagrees with the
	// directory name. The directory name is what the linker uses, so the
	// skill is invoked under a name its own file does not claim.
	IssueNameMismatch = "name_mismatch"
	// IssueNotLinked is a valid skill in the repo that is absent from a
	// runtime's skills home — present, committed, and not loadable.
	IssueNotLinked = "not_linked"
	// IssueShadowed is an identity skill whose name is also used by a
	// vendored skill. The linker walks identity first and vendor second, and
	// each pass replaces the link, so the VENDORED copy wins and the agent's
	// own skill is the one that disappears.
	IssueShadowed = "shadowed"
	// IssueDanglingLink is a symlink in a runtime home whose target no
	// longer exists — usually a skill deleted from the repo without a relink.
	IssueDanglingLink = "dangling_link"
	// IssueUnmanaged is a real directory in a runtime home that is not a
	// symlink into the identity repo. It works today and is invisible to
	// git, so it is lost the moment the agent is reprovisioned.
	IssueUnmanaged = "unmanaged"
	// IssueNotPushed is a skill that is in the identity repo but not yet in
	// GitHub — uncommitted, or committed and unpushed. It works right now and
	// dies with the pod. This exists because the platform links a new skill
	// automatically: without it, a skill that had never been pushed would
	// render as perfectly healthy right up until it vanished.
	IssueNotPushed = "not_pushed"
)

// Severity separates "this does not work" from "this works, but something is
// off". Without the split every finding renders at the same weight, and a skill
// missing a description reads as loudly as one no runtime can load — which
// blunts exactly the signal this scan exists to sharpen.
const (
	// SeverityError means the skill is not usable as reported.
	SeverityError = "error"
	// SeverityWarning means it works, but something will bite later.
	SeverityWarning = "warning"
)

// Issue is one problem found during a scan. Detail is human-readable and safe
// to render directly; it never contains file contents, only paths and names.
type Issue struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
}

// Skill is one skill package found in the identity repo.
type Skill struct {
	// Name is the directory name, which is what the runtime invokes.
	Name string `json:"name"`
	// Description comes from the SKILL.md frontmatter. Empty when the
	// frontmatter is missing or has no description.
	Description string `json:"description"`
	// Source is SourceIdentity or SourceVendor.
	Source string `json:"source"`
	// SourcePackage is the vendor package directory name; empty for
	// identity-owned skills.
	SourcePackage string `json:"sourcePackage,omitempty"`
	// Path is the skill directory: relative to the identity repo root for
	// identity and vendor skills, absolute for platform skills, which live
	// in the image rather than the repo.
	Path string `json:"path"`
	// Linked lists the runtimes this skill is actually loadable in, as
	// runtime identifiers. Empty means the skill exists but is dead.
	Linked []string `json:"linked"`
	// Issues are the problems specific to this skill.
	Issues []Issue `json:"issues,omitempty"`
}

// Healthy reports whether the scan found nothing at all to say about the skill.
func (s Skill) Healthy() bool { return len(s.Issues) == 0 }

// Broken reports whether the skill carries an error-severity issue — it exists
// but does not work.
func (s Skill) Broken() bool {
	for _, i := range s.Issues {
		if i.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Report is the full picture of one agent's skills at one moment. It is the
// wire shape POSTed to the control plane and served back by the read-only API.
type Report struct {
	Version int `json:"version"`
	// ReportedAt is set by the reporter at POST time (RFC3339). The scanner
	// leaves it empty so scans stay deterministic under test.
	ReportedAt string `json:"reportedAt,omitempty"`
	// Skills are sorted by source (identity first) then name.
	Skills []Skill `json:"skills"`
	// Issues are problems that belong to no single skill — stray state in a
	// runtime home rather than a defect in a repo skill.
	Issues []Issue `json:"issues,omitempty"`
}

// Options controls a scan. Only HomeDir is required; Scan does no environment
// lookups of its own so tests drive it entirely from a temp dir.
type Options struct {
	// RepoDir is the identity repo clone (e.g. /home/kyber/dev/dave-agent).
	// Empty when the agent has no identity repo, which is a supported
	// configuration: such an agent still has the image's platform skills, and
	// anything written into a runtime home by hand.
	RepoDir string
	// HomeDir is the agent's home, holding .claude/skills and .codex/skills.
	HomeDir string
	// PlatformDir holds the image-bundled capability skills. Defaults to
	// DefaultPlatformDir when empty. Links into this directory are expected
	// and healthy, not stray state.
	PlatformDir string
	// UnpushedPaths are repo-relative paths that exist locally but not in
	// GitHub — uncommitted, untracked, or committed-but-unpushed. Supplied by
	// the caller (which owns the git queries) so this package stays a pure
	// filesystem scan and stays testable without a repo. A skill is flagged
	// when any unpushed path falls inside its directory.
	UnpushedPaths []string
}

// Scan walks the identity repo and both runtime homes and returns what the
// agent actually has. It never fails on a malformed skill: a skill that cannot
// be read becomes a skill carrying an issue, because "we could not tell" and
// "there is nothing wrong" must not look the same in the UI.
//
// An absent or empty RepoDir is not an error either. An agent with no identity
// repo owns no skills, but it still has whatever the runtime image bundles, and
// anything hand-written into a runtime home — which is precisely the state
// worth surfacing, since none of it survives a reprovision.
func Scan(opts Options) (*Report, error) {
	if opts.HomeDir == "" {
		return nil, fmt.Errorf("skillscan: HomeDir is required")
	}
	if opts.PlatformDir == "" {
		opts.PlatformDir = DefaultPlatformDir
	}
	rep := &Report{Version: ReportVersion, Skills: []Skill{}}

	// Walk the same sources, in the same order, as the linker: the agent's
	// own skills first, then each vendored package. Order matters — it is
	// what decides which copy of a duplicated name wins.
	skills := collect(opts.RepoDir)

	// Resolve every runtime home once, then attribute links back to skills.
	// managed is the set of repo paths a runtime home currently points at.
	for i := range skills {
		abs := filepath.Join(opts.RepoDir, skills[i].Path)
		for _, rh := range runtimeHomes {
			link := filepath.Join(opts.HomeDir, rh.dir, skills[i].Name)
			if linkResolvesTo(link, abs) {
				skills[i].Linked = append(skills[i].Linked, rh.runtime)
			}
		}
	}

	// Shadowing: identity loses to vendor because the vendor pass relinks
	// last. Flag the identity skill, since that is the one that vanished.
	byName := map[string][]int{}
	for i, sk := range skills {
		byName[sk.Name] = append(byName[sk.Name], i)
	}
	for name, idxs := range byName {
		if len(idxs) < 2 {
			continue
		}
		winner := skills[idxs[len(idxs)-1]]
		for _, i := range idxs[:len(idxs)-1] {
			skills[i].Issues = append(skills[i].Issues, Issue{
				Code:     IssueShadowed,
				Severity: SeverityError,
				Detail: fmt.Sprintf("name %q is also provided by %s, which the linker applies last and therefore wins",
					name, describeSource(winner)),
			})
		}
	}

	// Durability: a skill that is not in GitHub dies with the pod. Checked
	// before the not_linked pass so both can be reported on one skill.
	for i := range skills {
		if skills[i].Source == SourcePlatform {
			continue // lives in the image, not the repo
		}
		if !anyPathUnder(opts.UnpushedPaths, skills[i].Path) {
			continue
		}
		skills[i].Issues = append(skills[i].Issues, Issue{
			Code:     IssueNotPushed,
			Severity: SeverityWarning,
			Detail: fmt.Sprintf("%s is not pushed to GitHub — it works in this pod and will not survive a reprovision; run `kyber-skills install`",
				skills[i].Path),
		})
	}

	// not_linked is only meaningful for a skill the linker would accept —
	// a directory with no SKILL.md is expected to be absent from the homes,
	// and reporting both issues would just be the same fact twice. A
	// shadowed skill is also expected to be missing, by definition.
	for i := range skills {
		if hasIssue(skills[i].Issues, IssueMissingSkillMD) || hasIssue(skills[i].Issues, IssueShadowed) {
			continue
		}
		// Platform skills are linked per-runtime by that runtime's own start
		// script, so a Claude-Code agent legitimately has the Telegram skill
		// in ~/.claude/skills and nowhere else. Only the identity linker
		// promises both homes, so only repo skills can fail that promise.
		if skills[i].Source == SourcePlatform {
			continue
		}
		for _, rh := range runtimeHomes {
			if !contains(skills[i].Linked, rh.runtime) {
				skills[i].Issues = append(skills[i].Issues, Issue{
					Code:     IssueNotLinked,
					Severity: SeverityError,
					Detail:   fmt.Sprintf("not loadable by %s — no link at ~/%s/%s; run `kyber-skills install`", rh.runtime, rh.dir, skills[i].Name),
				})
			}
		}
	}

	sort.SliceStable(skills, func(i, j int) bool {
		if skills[i].Source != skills[j].Source {
			return sourceRank(skills[i].Source) < sourceRank(skills[j].Source)
		}
		if skills[i].SourcePackage != skills[j].SourcePackage {
			return skills[i].SourcePackage < skills[j].SourcePackage
		}
		return skills[i].Name < skills[j].Name
	})
	platform, issues := scanRuntimeHomes(opts)
	// Platform skills are appended after the repo sort and then re-sorted, so
	// the final order is identity, vendor, platform — the order an operator
	// cares about, most-owned first.
	skills = append(skills, platform...)
	sort.SliceStable(skills, func(i, j int) bool {
		return sourceRank(skills[i].Source) < sourceRank(skills[j].Source)
	})
	rep.Skills = skills
	rep.Issues = issues
	return rep, nil
}

// sourceRank orders skills by how much they belong to the agent: its own
// first, then what it vendored, then what the platform gave it.
func sourceRank(source string) int {
	switch source {
	case SourceIdentity:
		return 0
	case SourceVendor:
		return 1
	default:
		return 2
	}
}

// collect walks <repo>/skills and <repo>/vendor/*/skills in linker order and
// returns one Skill per directory found, each already carrying its own
// content-level issues.
func collect(repoDir string) []Skill {
	if repoDir == "" {
		return nil
	}
	var out []Skill
	sources := []struct {
		rel     string
		source  string
		pkgName string
	}{{rel: "skills", source: SourceIdentity}}

	vendorEntries, _ := os.ReadDir(filepath.Join(repoDir, "vendor"))
	for _, ve := range vendorEntries {
		if !ve.IsDir() {
			continue
		}
		sources = append(sources, struct {
			rel     string
			source  string
			pkgName string
		}{
			rel:     filepath.Join("vendor", ve.Name(), "skills"),
			source:  SourceVendor,
			pkgName: ve.Name(),
		})
	}

	for _, src := range sources {
		entries, err := os.ReadDir(filepath.Join(repoDir, src.rel))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if isHidden(e.Name()) {
				continue
			}
			// Follow symlinked skill directories: a vendored tree is
			// sometimes a link, and os.ReadDir reports the link itself.
			info, err := os.Stat(filepath.Join(repoDir, src.rel, e.Name()))
			if err != nil || !info.IsDir() {
				continue
			}
			sk := Skill{
				Name:          e.Name(),
				Source:        src.source,
				SourcePackage: src.pkgName,
				Path:          filepath.Join(src.rel, e.Name()),
				Linked:        []string{},
			}
			readSkillMD(filepath.Join(repoDir, sk.Path, "SKILL.md"), &sk)
			out = append(out, sk)
		}
	}
	return out
}

// frontmatter is the subset of SKILL.md's YAML header the platform depends on.
// Unknown keys are ignored — a skill may carry whatever else its runtime reads.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// maxSkillMDBytes caps how much of a SKILL.md is read while looking for
// frontmatter. The header sits at the top of the file; the body can be
// arbitrarily long and is never needed here.
const maxSkillMDBytes = 64 << 10

// readSkillMD fills sk.Description and appends any content-level issues.
func readSkillMD(path string, sk *Skill) {
	f, err := os.Open(path)
	if err != nil {
		sk.Issues = append(sk.Issues, Issue{
			Code:     IssueMissingSkillMD,
			Severity: SeverityError,
			Detail:   fmt.Sprintf("%s has no SKILL.md, so the linker skips it and the skill does not exist at runtime", sk.Path),
		})
		return
	}
	defer f.Close()

	buf := make([]byte, maxSkillMDBytes)
	n, _ := f.Read(buf)
	body := string(buf[:n])

	raw, ok := extractFrontmatter(body)
	if !ok {
		sk.Issues = append(sk.Issues, Issue{
			Code:     IssueInvalidFrontmatter,
			Severity: SeverityError,
			Detail:   "SKILL.md has no YAML frontmatter block (expected a leading '---' fenced header)",
		})
		return
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(raw), &fm); err != nil {
		sk.Issues = append(sk.Issues, Issue{
			Code:     IssueInvalidFrontmatter,
			Severity: SeverityError,
			Detail:   "SKILL.md frontmatter is not valid YAML",
		})
		return
	}
	sk.Description = strings.TrimSpace(fm.Description)
	if sk.Description == "" {
		sk.Issues = append(sk.Issues, Issue{
			Code:     IssueMissingDescription,
			Severity: SeverityWarning,
			Detail:   "frontmatter has no description, so the skill can only be invoked explicitly and will never trigger on its own",
		})
	}
	if name := strings.TrimSpace(fm.Name); name != "" && name != sk.Name {
		sk.Issues = append(sk.Issues, Issue{
			Code:     IssueNameMismatch,
			Severity: SeverityWarning,
			Detail: fmt.Sprintf("frontmatter name is %q but the directory is %q; the directory name is what the runtime invokes",
				name, sk.Name),
		})
	}
}

// extractFrontmatter returns the YAML between the leading '---' fence and the
// next '---' line. Leading blank lines and a UTF-8 BOM are tolerated.
func extractFrontmatter(body string) (string, bool) {
	body = strings.TrimPrefix(body, "\ufeff")
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "---" {
		return "", false
	}
	for j := i + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "---" {
			return strings.Join(lines[i+1:j], "\n"), true
		}
	}
	return "", false
}

// scanRuntimeHomes walks both runtime skill homes and accounts for everything
// that is NOT a link into the identity repo.
//
// Three outcomes are possible. A link into the image's platform skills
// directory is a bundled capability cookbook — a real skill the agent has, so
// it is returned as one. A link whose target is gone is dangling. Anything
// else — a real directory, or a link pointing somewhere unmanaged — is state
// that works today and is committed nowhere, so it disappears the moment the
// pod is reprovisioned. That last case is the quiet one: it looks completely
// healthy from inside the pod, right up until the skill is gone.
func scanRuntimeHomes(opts Options) ([]Skill, []Issue) {
	var issues []Issue
	// Platform skills appear once per runtime home; collapse them into one
	// entry carrying the set of runtimes they are live in.
	platform := map[string]*Skill{}
	var order []string

	for _, rh := range runtimeHomes {
		dir := filepath.Join(opts.HomeDir, rh.dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			// A runtime owns the dot-entries in its own skills home — Codex
			// keeps a `.system` directory there. They are not skills (nothing
			// can invoke a name starting with a dot), so reporting them as
			// stray state would put a permanent warning on every Codex agent.
			// Found exactly that way, on a real agent.
			if isHidden(e.Name()) {
				continue
			}
			full := filepath.Join(dir, e.Name())
			li, err := os.Lstat(full)
			if err != nil {
				continue
			}
			isLink := li.Mode()&os.ModeSymlink != 0
			if !isLink {
				if e.IsDir() {
					issues = append(issues, Issue{
						Code:     IssueUnmanaged,
						Severity: SeverityWarning,
						Detail: fmt.Sprintf("~/%s/%s is a real directory, not a link into the identity repo — it is committed nowhere and will not survive a reprovision",
							rh.dir, e.Name()),
					})
				}
				continue
			}
			target, err := filepath.EvalSymlinks(full)
			if err != nil {
				issues = append(issues, Issue{
					Code:     IssueDanglingLink,
					Severity: SeverityWarning,
					Detail:   fmt.Sprintf("~/%s/%s points at a target that no longer exists", rh.dir, e.Name()),
				})
				continue
			}
			if underDir(target, opts.RepoDir) {
				continue // an identity or vendor skill; already reported
			}
			if underDir(target, opts.PlatformDir) {
				sk, ok := platform[e.Name()]
				if !ok {
					sk = &Skill{
						Name:   e.Name(),
						Source: SourcePlatform,
						Path:   target,
						Linked: []string{},
					}
					readSkillMD(filepath.Join(target, "SKILL.md"), sk)
					platform[e.Name()] = sk
					order = append(order, e.Name())
				}
				sk.Linked = append(sk.Linked, rh.runtime)
				continue
			}
			issues = append(issues, Issue{
				Code:     IssueUnmanaged,
				Severity: SeverityWarning,
				Detail: fmt.Sprintf("~/%s/%s links outside the identity repo (%s) — it is committed nowhere and will not survive a reprovision",
					rh.dir, e.Name(), target),
			})
		}
	}

	sort.Strings(order)
	out := make([]Skill, 0, len(order))
	for _, name := range order {
		out = append(out, *platform[name])
	}
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Code != issues[j].Code {
			return issues[i].Code < issues[j].Code
		}
		return issues[i].Detail < issues[j].Detail
	})
	return out, issues
}

// linkResolvesTo reports whether link is a symlink (or path) that resolves to
// the same directory as want.
func linkResolvesTo(link, want string) bool {
	got, err := filepath.EvalSymlinks(link)
	if err != nil {
		return false
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		return false
	}
	return got == wantResolved
}

// underDir reports whether path is dir or lives beneath it. Both are resolved
// first so a symlinked home does not produce a false negative. An empty dir is
// never a container: with no identity repo, filepath.Rel("", …) would otherwise
// report every absolute path as living under it and hide real findings.
func underDir(path, dir string) bool {
	if dir == "" {
		return false
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolvedDir = dir
	}
	rel, err := filepath.Rel(resolvedDir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func describeSource(s Skill) string {
	if s.Source == SourceVendor {
		return fmt.Sprintf("vendor package %q", s.SourcePackage)
	}
	return "the agent's own skills/"
}

// anyPathUnder reports whether any of paths is dir itself or lives beneath it.
// Paths are repo-relative and slash-separated, as git reports them.
func anyPathUnder(paths []string, dir string) bool {
	prefix := dir + "/"
	for _, p := range paths {
		if p == dir || strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// isHidden reports whether a directory entry is a dot-entry, which belongs to
// the tool that made it rather than to the agent.
func isHidden(name string) bool { return strings.HasPrefix(name, ".") }

func hasIssue(issues []Issue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
