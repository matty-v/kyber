package skillscan_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matty-v/kyber/pkg/skillscan"
)

// fixture builds a repo + home pair on disk and returns both paths. Tests
// assert against the real filesystem rather than a fake, because the whole
// point of this package is that the filesystem is the only honest source: the
// linker, the runtimes, and this scanner all agree only if the actual symlinks
// are right (kyber#691).
type fixture struct {
	repo string
	home string
	t    *testing.T
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{repo: filepath.Join(root, "repo"), home: filepath.Join(root, "home"), t: t}
	for _, d := range []string{
		filepath.Join(f.repo, "skills"),
		filepath.Join(f.home, ".claude", "skills"),
		filepath.Join(f.home, ".codex", "skills"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return f
}

// skill writes <repo>/skills/<name>/SKILL.md with the given frontmatter body.
func (f *fixture) skill(name, frontmatter string) string {
	f.t.Helper()
	return f.skillAt(filepath.Join("skills", name), frontmatter)
}

// vendorSkill writes <repo>/vendor/<pkg>/skills/<name>/SKILL.md.
func (f *fixture) vendorSkill(pkg, name, frontmatter string) string {
	f.t.Helper()
	return f.skillAt(filepath.Join("vendor", pkg, "skills", name), frontmatter)
}

func (f *fixture) skillAt(rel, frontmatter string) string {
	f.t.Helper()
	dir := filepath.Join(f.repo, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatalf("mkdir %s: %v", dir, err)
	}
	if frontmatter != "" {
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(frontmatter), 0o644); err != nil {
			f.t.Fatalf("write SKILL.md: %v", err)
		}
	}
	return dir
}

// link mimics exactly what images/shared/kyber-identity-repo.sh does: symlink
// the skill directory into both runtime homes under its directory name.
func (f *fixture) link(name, target string, runtimes ...string) {
	f.t.Helper()
	if len(runtimes) == 0 {
		runtimes = []string{".claude", ".codex"}
	}
	for _, rt := range runtimes {
		dst := filepath.Join(f.home, rt, "skills", name)
		_ = os.RemoveAll(dst)
		if err := os.Symlink(target, dst); err != nil {
			f.t.Fatalf("symlink %s: %v", dst, err)
		}
	}
}

func (f *fixture) scan() *skillscan.Report {
	f.t.Helper()
	rep, err := skillscan.Scan(skillscan.Options{RepoDir: f.repo, HomeDir: f.home})
	if err != nil {
		f.t.Fatalf("Scan: %v", err)
	}
	return rep
}

func findSkill(t *testing.T, rep *skillscan.Report, name string) skillscan.Skill {
	t.Helper()
	for _, s := range rep.Skills {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("skill %q not found in report (have %v)", name, names(rep))
	return skillscan.Skill{}
}

func names(rep *skillscan.Report) []string {
	var out []string
	for _, s := range rep.Skills {
		out = append(out, s.Name)
	}
	return out
}

func codes(issues []skillscan.Issue) []string {
	var out []string
	for _, i := range issues {
		out = append(out, i.Code)
	}
	return out
}

func hasCode(issues []skillscan.Issue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

const goodFrontmatter = `---
name: deploy
description: Ship the thing to prod.
---

Body text.
`

func TestScan_HealthySkillIsLinkedInBothRuntimes(t *testing.T) {
	f := newFixture(t)
	dir := f.skill("deploy", goodFrontmatter)
	f.link("deploy", dir)

	sk := findSkill(t, f.scan(), "deploy")
	if sk.Description != "Ship the thing to prod." {
		t.Errorf("description: got %q", sk.Description)
	}
	if sk.Source != skillscan.SourceIdentity {
		t.Errorf("source: got %q, want %q", sk.Source, skillscan.SourceIdentity)
	}
	if len(sk.Linked) != 2 {
		t.Errorf("linked: got %v, want both runtimes", sk.Linked)
	}
	if !sk.Healthy() {
		t.Errorf("expected healthy, got issues %v", codes(sk.Issues))
	}
}

func TestScan_LegacyFlatSkillCompatibilityWrapperIsManaged(t *testing.T) {
	f := newFixture(t)
	source := filepath.Join(f.repo, "skills", "approve.md")
	body := "---\nname: approve\ndescription: Approve a plan.\n---\nbody\n"
	if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []string{".claude", ".codex"} {
		wrapper := filepath.Join(f.home, runtime, "skills", "approve")
		if err := os.MkdirAll(wrapper, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(source, filepath.Join(wrapper, "SKILL.md")); err != nil {
			t.Fatal(err)
		}
	}

	rep := f.scan()
	sk := findSkill(t, rep, "approve")
	if sk.Path != filepath.Join("skills", "approve.md") || len(sk.Linked) != 2 {
		t.Fatalf("flat skill = %+v", sk)
	}
	if !sk.Healthy() || len(rep.Issues) != 0 {
		t.Fatalf("compatibility wrapper reported unhealthy: skill=%+v report=%+v", sk.Issues, rep.Issues)
	}
}

func TestScan_CanonicalPackageTakesPrecedenceOverFlatSkill(t *testing.T) {
	f := newFixture(t)
	f.skill("approve", "---\nname: approve\ndescription: Canonical package.\n---\n")
	flat := filepath.Join(f.repo, "skills", "approve.md")
	if err := os.WriteFile(flat, []byte("---\nname: approve\ndescription: Legacy copy.\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.link("approve", filepath.Join(f.repo, "skills", "approve"))
	rep := f.scan()
	canonical := findSkill(t, rep, "approve")
	if canonical.Path != "skills/approve" || canonical.Description != "Canonical package." || len(canonical.Linked) != 2 {
		t.Fatalf("canonical package did not win: %+v", canonical)
	}
	for _, s := range rep.Skills {
		if s.Path == "skills/approve.md" && len(s.Linked) != 0 {
			t.Fatalf("flat predecessor unexpectedly linked: %+v", s)
		}
	}
}

// The regression that motivates the whole feature: the skill is committed and
// present in the repo, and no runtime can load it. Before this scanner, every
// reporting surface called that state healthy.
func TestScan_InRepoButNotLinked(t *testing.T) {
	f := newFixture(t)
	f.skill("deploy", goodFrontmatter)

	sk := findSkill(t, f.scan(), "deploy")
	if len(sk.Linked) != 0 {
		t.Fatalf("linked: got %v, want none", sk.Linked)
	}
	if !hasCode(sk.Issues, skillscan.IssueNotLinked) {
		t.Fatalf("expected %s, got %v", skillscan.IssueNotLinked, codes(sk.Issues))
	}
	// One issue per runtime, so the UI can say which half is broken.
	var n int
	for _, i := range sk.Issues {
		if i.Code == skillscan.IssueNotLinked {
			n++
		}
	}
	if n != 2 {
		t.Errorf("not_linked count: got %d, want 2 (one per runtime)", n)
	}
}

func TestScan_LinkedInOneRuntimeOnly(t *testing.T) {
	f := newFixture(t)
	dir := f.skill("deploy", goodFrontmatter)
	f.link("deploy", dir, ".claude")

	sk := findSkill(t, f.scan(), "deploy")
	if len(sk.Linked) != 1 || sk.Linked[0] != skillscan.RuntimeClaudeCode {
		t.Fatalf("linked: got %v, want [claude-code]", sk.Linked)
	}
	var detail string
	for _, i := range sk.Issues {
		if i.Code == skillscan.IssueNotLinked {
			detail = i.Detail
		}
	}
	if !strings.Contains(detail, skillscan.RuntimeCodex) {
		t.Errorf("expected the codex gap named in the detail; got %q", detail)
	}
}

func TestScan_MissingSkillMD(t *testing.T) {
	f := newFixture(t)
	f.skillAt(filepath.Join("skills", "halfbaked"), "")

	sk := findSkill(t, f.scan(), "halfbaked")
	if !hasCode(sk.Issues, skillscan.IssueMissingSkillMD) {
		t.Fatalf("expected %s, got %v", skillscan.IssueMissingSkillMD, codes(sk.Issues))
	}
	// A directory the linker skips is EXPECTED to be unlinked; reporting
	// not_linked too would just restate the same fact and add noise.
	if hasCode(sk.Issues, skillscan.IssueNotLinked) {
		t.Errorf("missing SKILL.md should not also report not_linked; got %v", codes(sk.Issues))
	}
}

func TestScan_FrontmatterProblems(t *testing.T) {
	tests := []struct {
		name     string
		dir      string
		content  string
		wantCode string
	}{
		{
			name:     "no frontmatter fence",
			dir:      "nofence",
			content:  "Just a markdown file with no header.\n",
			wantCode: skillscan.IssueInvalidFrontmatter,
		},
		{
			name:     "unterminated fence",
			dir:      "unterminated",
			content:  "---\nname: unterminated\ndescription: x\n",
			wantCode: skillscan.IssueInvalidFrontmatter,
		},
		{
			name:     "not valid yaml",
			dir:      "badyaml",
			content:  "---\nname: [unclosed\n---\nbody\n",
			wantCode: skillscan.IssueInvalidFrontmatter,
		},
		{
			name:     "no description",
			dir:      "nodesc",
			content:  "---\nname: nodesc\n---\nbody\n",
			wantCode: skillscan.IssueMissingDescription,
		},
		{
			name:     "name disagrees with directory",
			dir:      "ondisk",
			content:  "---\nname: something-else\ndescription: x\n---\nbody\n",
			wantCode: skillscan.IssueNameMismatch,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			dir := f.skill(tc.dir, tc.content)
			f.link(tc.dir, dir)

			sk := findSkill(t, f.scan(), tc.dir)
			if !hasCode(sk.Issues, tc.wantCode) {
				t.Fatalf("expected %s, got %v", tc.wantCode, codes(sk.Issues))
			}
		})
	}
}

func TestScan_LeadingBlankLinesAndBOMStillParse(t *testing.T) {
	f := newFixture(t)
	dir := f.skill("bommy", "\ufeff\n\n---\nname: bommy\ndescription: Survives a BOM.\n---\nbody\n")
	f.link("bommy", dir)

	sk := findSkill(t, f.scan(), "bommy")
	if sk.Description != "Survives a BOM." {
		t.Fatalf("description: got %q, issues %v", sk.Description, codes(sk.Issues))
	}
}

// The linker walks identity/ first and vendor/ second, and each pass does
// `rm -rf` + `ln -sf`, so a duplicated name means the VENDORED copy wins and
// the agent's own skill is the one that silently disappears.
func TestScan_VendorShadowsIdentitySkill(t *testing.T) {
	f := newFixture(t)
	f.skill("restart", goodFrontmatter)
	vdir := f.vendorSkill("falcon-dev-common", "restart", "---\nname: restart\ndescription: Vendored restart.\n---\n")
	f.link("restart", vdir)

	rep := f.scan()
	var identity, vendor skillscan.Skill
	for _, s := range rep.Skills {
		if s.Name != "restart" {
			continue
		}
		if s.Source == skillscan.SourceIdentity {
			identity = s
		} else {
			vendor = s
		}
	}
	if !hasCode(identity.Issues, skillscan.IssueShadowed) {
		t.Fatalf("expected the identity copy flagged %s; got %v", skillscan.IssueShadowed, codes(identity.Issues))
	}
	if vendor.SourcePackage != "falcon-dev-common" {
		t.Errorf("vendor package: got %q", vendor.SourcePackage)
	}
	if !vendor.Healthy() {
		t.Errorf("the winning vendor copy is loadable and should be clean; got %v", codes(vendor.Issues))
	}
	// The shadowed copy is expected to be unlinked — that IS the shadowing.
	if hasCode(identity.Issues, skillscan.IssueNotLinked) {
		t.Errorf("shadowed skill should not also report not_linked; got %v", codes(identity.Issues))
	}
}

// A skill the agent hand-wrote straight into ~/.claude/skills works right now
// and is invisible to git, so it dies at the next reprovision. That is the
// exact shape that hid a missing platform fix on hk-47.
func TestScan_UnmanagedDirectoryInRuntimeHome(t *testing.T) {
	f := newFixture(t)
	stray := filepath.Join(f.home, ".claude", "skills", "handwritten")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "SKILL.md"), []byte(goodFrontmatter), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := f.scan()
	if !hasCode(rep.Issues, skillscan.IssueUnmanaged) {
		t.Fatalf("expected report-level %s, got %v", skillscan.IssueUnmanaged, codes(rep.Issues))
	}
	if len(rep.Skills) != 0 {
		t.Errorf("an unmanaged dir is not a repo skill; got %v", names(rep))
	}
}

func TestScan_LinkOutsideRepoIsUnmanaged(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	f.link("borrowed", outside, ".codex")

	rep := f.scan()
	if !hasCode(rep.Issues, skillscan.IssueUnmanaged) {
		t.Fatalf("expected %s, got %v", skillscan.IssueUnmanaged, codes(rep.Issues))
	}
}

func TestScan_DanglingLink(t *testing.T) {
	f := newFixture(t)
	dir := f.skill("deleted", goodFrontmatter)
	f.link("deleted", dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	rep := f.scan()
	if !hasCode(rep.Issues, skillscan.IssueDanglingLink) {
		t.Fatalf("expected %s, got %v", skillscan.IssueDanglingLink, codes(rep.Issues))
	}
}

func TestScan_NoIdentityRepoIsNotAnError(t *testing.T) {
	root := t.TempDir()
	rep, err := skillscan.Scan(skillscan.Options{
		RepoDir: filepath.Join(root, "does-not-exist"),
		HomeDir: filepath.Join(root, "also-missing"),
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(rep.Skills) != 0 || len(rep.Issues) != 0 {
		t.Errorf("expected an empty report, got %d skills / %d issues", len(rep.Skills), len(rep.Issues))
	}
	if rep.Version != skillscan.ReportVersion {
		t.Errorf("version: got %d, want %d", rep.Version, skillscan.ReportVersion)
	}
}

// An agent configured with NO identity repo is a supported setup, and it still
// has whatever the runtime image bundles plus anything hand-written into a
// runtime home. Refusing to scan without a repo would leave exactly those
// agents invisible — the blind spot this feature exists to remove.
func TestScan_NoRepoDirStillReportsPlatformAndUnmanagedSkills(t *testing.T) {
	f := newFixture(t)
	platform := filepath.Join(t.TempDir(), "opt-kyber-skills")
	tg := filepath.Join(platform, "telegram-messaging")
	if err := os.MkdirAll(tg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tg, "SKILL.md"),
		[]byte("---\nname: telegram-messaging\ndescription: Talk on Telegram.\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.link("telegram-messaging", tg, ".claude")
	stray := filepath.Join(f.home, ".codex", "skills", "handwritten")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}

	rep, err := skillscan.Scan(skillscan.Options{HomeDir: f.home, PlatformDir: platform})
	if err != nil {
		t.Fatalf("Scan with no RepoDir: %v", err)
	}
	sk := findSkill(t, rep, "telegram-messaging")
	if sk.Source != skillscan.SourcePlatform || !sk.Healthy() {
		t.Errorf("platform skill = %+v", sk)
	}
	if !hasCode(rep.Issues, skillscan.IssueUnmanaged) {
		t.Errorf("expected %s for the hand-written directory; got %v", skillscan.IssueUnmanaged, codes(rep.Issues))
	}
}

func TestScan_RequiresHomeDir(t *testing.T) {
	if _, err := skillscan.Scan(skillscan.Options{RepoDir: "/tmp"}); err == nil {
		t.Error("expected an error when HomeDir is empty")
	}
	if _, err := skillscan.Scan(skillscan.Options{HomeDir: "/tmp"}); err != nil {
		t.Errorf("an empty RepoDir must be accepted: %v", err)
	}
}

func TestScan_SortsIdentityBeforeVendor(t *testing.T) {
	f := newFixture(t)
	f.skill("zulu", goodFrontmatter)
	f.vendorSkill("common", "alpha", "---\nname: alpha\ndescription: x\n---\n")

	rep := f.scan()
	if got := names(rep); len(got) != 2 || got[0] != "zulu" || got[1] != "alpha" {
		t.Errorf("order: got %v, want identity skills first", got)
	}
}

// The runtime images bake capability cookbooks into /opt/kyber/skills and the
// start scripts link them in only when the matching sidecar exists. Those are
// real skills the agent has — reporting them as stray state would have put a
// scary warning on every Telegram-enabled agent in the fleet.
func TestScan_PlatformSkillIsReportedNotFlagged(t *testing.T) {
	f := newFixture(t)
	platform := filepath.Join(t.TempDir(), "opt-kyber-skills")
	tg := filepath.Join(platform, "telegram-messaging")
	if err := os.MkdirAll(tg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tg, "SKILL.md"),
		[]byte("---\nname: telegram-messaging\ndescription: Talk on Telegram.\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only Claude Code links it — that is how start-claude.sh behaves.
	f.link("telegram-messaging", tg, ".claude")

	rep, err := skillscan.Scan(skillscan.Options{RepoDir: f.repo, HomeDir: f.home, PlatformDir: platform})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Issues) != 0 {
		t.Fatalf("platform skills must not be reported as problems; got %v", codes(rep.Issues))
	}
	sk := findSkill(t, rep, "telegram-messaging")
	if sk.Source != skillscan.SourcePlatform {
		t.Errorf("source: got %q, want %q", sk.Source, skillscan.SourcePlatform)
	}
	if sk.Description != "Talk on Telegram." {
		t.Errorf("description: got %q", sk.Description)
	}
	if len(sk.Linked) != 1 || sk.Linked[0] != skillscan.RuntimeClaudeCode {
		t.Errorf("linked: got %v, want [claude-code]", sk.Linked)
	}
	// A platform skill present in one runtime home is correct by design, so
	// it must NOT be reported as missing from the other.
	if !sk.Healthy() {
		t.Errorf("expected a clean platform skill; got %v", codes(sk.Issues))
	}
}

func TestScan_PlatformSkillLinkedInBothRuntimes(t *testing.T) {
	f := newFixture(t)
	platform := filepath.Join(t.TempDir(), "opt-kyber-skills")
	dir := filepath.Join(platform, "discord-messaging")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: discord-messaging\ndescription: Talk on Discord.\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.link("discord-messaging", dir)

	rep, err := skillscan.Scan(skillscan.Options{RepoDir: f.repo, HomeDir: f.home, PlatformDir: platform})
	if err != nil {
		t.Fatal(err)
	}
	sk := findSkill(t, rep, "discord-messaging")
	if len(sk.Linked) != 2 {
		t.Errorf("linked: got %v, want both runtimes", sk.Linked)
	}
	// One entry, not one per runtime home.
	var n int
	for _, s := range rep.Skills {
		if s.Name == "discord-messaging" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("platform skill appeared %d times, want 1", n)
	}
}

func TestScan_OrdersIdentityThenVendorThenPlatform(t *testing.T) {
	f := newFixture(t)
	platform := filepath.Join(t.TempDir(), "opt-kyber-skills")
	pdir := filepath.Join(platform, "bundled")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "SKILL.md"),
		[]byte("---\nname: bundled\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.link("bundled", pdir)
	f.skill("mine", goodFrontmatter)
	f.vendorSkill("common", "borrowed", "---\nname: borrowed\ndescription: x\n---\n")

	rep, err := skillscan.Scan(skillscan.Options{RepoDir: f.repo, HomeDir: f.home, PlatformDir: platform})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mine", "borrowed", "bundled"}
	got := names(rep)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v, want %v", got, want)
		}
	}
}

// Severity is what keeps the signal sharp: "no runtime can load this" and
// "this has no description" must not render at the same weight.
func TestScan_SeveritySeparatesBrokenFromMerelyImperfect(t *testing.T) {
	f := newFixture(t)

	// Works everywhere, but the frontmatter is incomplete.
	warn := f.skill("warny", "---\nname: mismatched\n---\nbody\n")
	f.link("warny", warn)
	// Committed and loadable by nothing, with otherwise perfect frontmatter
	// so the only findings are the unloadable ones.
	f.skill("broken", "---\nname: broken\ndescription: Committed, loadable by nothing.\n---\nbody\n")

	rep := f.scan()

	warned := findSkill(t, rep, "warny")
	if warned.Broken() {
		t.Errorf("a description/name problem must not read as broken; got %v", codes(warned.Issues))
	}
	if warned.Healthy() {
		t.Error("expected the warnings to still be reported")
	}
	for _, iss := range warned.Issues {
		if iss.Severity != skillscan.SeverityWarning {
			t.Errorf("%s: severity %q, want warning", iss.Code, iss.Severity)
		}
	}

	brokenSkill := findSkill(t, rep, "broken")
	if !brokenSkill.Broken() {
		t.Fatalf("an unloadable skill must be broken; got %v", codes(brokenSkill.Issues))
	}
	for _, iss := range brokenSkill.Issues {
		if iss.Severity != skillscan.SeverityError {
			t.Errorf("%s: severity %q, want error", iss.Code, iss.Severity)
		}
	}
}

// Every issue the scanner can emit must carry a severity — an unset one would
// be normalized to "error" by the control plane and quietly overstate a
// harmless finding.
func TestScan_EveryIssueCarriesASeverity(t *testing.T) {
	f := newFixture(t)
	f.skill("nofrontmatter", "no header here\n")
	f.skillAt(filepath.Join("skills", "empty"), "")
	f.skill("nodesc", "---\nname: nodesc\n---\n")
	f.skill("mismatch", "---\nname: other\ndescription: x\n---\n")
	f.skill("dupe", goodFrontmatter)
	f.vendorSkill("pkg", "dupe", "---\nname: dupe\ndescription: x\n---\n")
	stray := filepath.Join(f.home, ".claude", "skills", "handwritten")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	gone := f.skill("gone", goodFrontmatter)
	f.link("gone", gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	rep := f.scan()
	seen := map[string]bool{}
	check := func(issues []skillscan.Issue) {
		for _, iss := range issues {
			seen[iss.Code] = true
			if iss.Severity != skillscan.SeverityError && iss.Severity != skillscan.SeverityWarning {
				t.Errorf("issue %q has severity %q, want error or warning", iss.Code, iss.Severity)
			}
		}
	}
	for _, sk := range rep.Skills {
		check(sk.Issues)
	}
	check(rep.Issues)

	// Guard against the fixture drifting until it stops producing issues at
	// all, which would make this test pass vacuously.
	for _, want := range []string{
		skillscan.IssueMissingSkillMD, skillscan.IssueInvalidFrontmatter,
		skillscan.IssueMissingDescription, skillscan.IssueNameMismatch,
		skillscan.IssueNotLinked, skillscan.IssueShadowed,
		skillscan.IssueDanglingLink, skillscan.IssueUnmanaged,
	} {
		if !seen[want] {
			t.Errorf("fixture no longer produces %s — this test is not covering it", want)
		}
	}
}

// Codex keeps a `.system` directory inside its own skills home. Nothing can
// invoke a name starting with a dot, so it is not a skill — and reporting it as
// stray state put a permanent, unfixable warning on every Codex agent. Found on
// a real agent in the dev instance, not in review.
func TestScan_RuntimeOwnedDotEntriesAreNotSkillsOrIssues(t *testing.T) {
	f := newFixture(t)
	for _, home := range []string{".claude", ".codex"} {
		if err := os.MkdirAll(filepath.Join(f.home, home, "skills", ".system"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A dot-entry in the repo's skills/ is equally not a skill.
	if err := os.MkdirAll(filepath.Join(f.repo, "skills", ".cache"), 0o755); err != nil {
		t.Fatal(err)
	}

	rep := f.scan()
	if len(rep.Issues) != 0 {
		t.Errorf("dot-entries must not be reported as problems; got %v", rep.Issues)
	}
	if len(rep.Skills) != 0 {
		t.Errorf("dot-entries must not be reported as skills; got %v", names(rep))
	}
}

// Linking is automatic now, so a skill starts working the moment it is
// written. Durability has to be reported separately or a brand-new skill that
// had never been pushed would render as perfectly healthy right up until the
// pod was reprovisioned and it vanished.
func TestScan_NotPushedIsReportedPerSkill(t *testing.T) {
	f := newFixture(t)
	saved := f.skill("saved", goodFrontmatter)
	draft := f.skill("draft", "---\nname: draft\ndescription: Not in GitHub yet.\n---\n")
	f.link("saved", saved)
	f.link("draft", draft)

	rep, err := skillscan.Scan(skillscan.Options{
		RepoDir: f.repo, HomeDir: f.home,
		UnpushedPaths: []string{"skills/draft/SKILL.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := findSkill(t, rep, "draft")
	if !hasCode(got.Issues, skillscan.IssueNotPushed) {
		t.Fatalf("expected %s; got %v", skillscan.IssueNotPushed, codes(got.Issues))
	}
	if got.Broken() {
		t.Error("not being pushed is a warning — the skill works right now")
	}
	// The neighbouring skill shares a parent directory and must not be caught
	// by a sloppy prefix match.
	if other := findSkill(t, rep, "saved"); hasCode(other.Issues, skillscan.IssueNotPushed) {
		t.Errorf("a pushed skill was flagged; got %v", codes(other.Issues))
	}
}

// Platform skills live in the image, not the repo, so "is it pushed" is not a
// question that applies to them.
func TestScan_PlatformSkillsAreNeverFlaggedNotPushed(t *testing.T) {
	f := newFixture(t)
	platform := filepath.Join(t.TempDir(), "opt-kyber-skills")
	dir := filepath.Join(platform, "telegram-messaging")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: telegram-messaging\ndescription: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.link("telegram-messaging", dir, ".claude")

	rep, err := skillscan.Scan(skillscan.Options{
		RepoDir: f.repo, HomeDir: f.home, PlatformDir: platform,
		// A deliberately over-broad list: even so, a platform skill is not
		// a repo path and must not match.
		UnpushedPaths: []string{"skills", "telegram-messaging"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sk := findSkill(t, rep, "telegram-messaging"); hasCode(sk.Issues, skillscan.IssueNotPushed) {
		t.Errorf("platform skill flagged %s; got %v", skillscan.IssueNotPushed, codes(sk.Issues))
	}
}

// An empty report must serialize as `[]`, not `null`. Every consumer would
// otherwise need a null branch to express "this agent has no skills" — which
// is a real answer, not a missing one.
func TestScan_EmptyReportSerializesAsEmptyArrays(t *testing.T) {
	root := t.TempDir()
	rep, err := skillscan.Scan(skillscan.Options{
		RepoDir: filepath.Join(root, "no-repo"),
		HomeDir: filepath.Join(root, "no-home"),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `"skills":[]`) {
		t.Errorf("skills serialized as null: %s", got)
	}
	if strings.Contains(got, "null") {
		t.Errorf("report contains a null: %s", got)
	}
}
