// Command kyber-skills is the in-pod tool for an agent's skills.
//
// Every Kyber agent keeps its skills in one predictable place — the identity
// repo, as skills/<name>/SKILL.md — and the boot/sync linker symlinks each
// package into both runtime homes (~/.claude/skills, ~/.codex/skills) so either
// runtime can load it. This binary is what keeps that contract true between
// boots and what makes it visible outside the pod.
//
//	kyber-skills install   normalize, link, commit, push, then report.
//	                       The one command to run after writing or downloading
//	                       a skill — it is idempotent, so running it twice is
//	                       always safe.
//	kyber-skills list      print what this agent has, and what is wrong with it.
//	kyber-skills report    scan and push the report to the control plane.
//	                       Called by the identity-repo sync at boot and on
//	                       every restart-session.
//
// The report goes through the status sidecar's localhost forwarder, which
// supplies agent identity and auth — this binary never needs a control-plane
// URL or a pod token.
//
// Nothing here ever exits non-zero for a broken skill. A malformed skill is a
// finding to surface, not a reason to fail an agent's boot.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/matty-v/kyber/pkg/skillscan"
)

const (
	defaultSidecarURL = "http://127.0.0.1:8091"
	postTimeout       = 10 * time.Second
	gitTimeout        = 60 * time.Second
)

// runtimeSkillDirs are the two runtime homes the linker maintains, relative to
// the agent's home. Kept in lockstep with images/shared/kyber-identity-repo.sh:
// if a runtime is added there, add it here or newly-installed skills will be
// live in one runtime and dead in the other.
var runtimeSkillDirs = []string{
	filepath.Join(".claude", "skills"),
	filepath.Join(".codex", "skills"),
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "report":
		os.Exit(runReport(args))
	case "list":
		os.Exit(runList(args))
	case "install":
		os.Exit(runInstall(args))
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "kyber-skills: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `kyber-skills — manage and report this agent's skills

  kyber-skills install [--from PATH] [--name NAME] [--no-push]
        Link every skill in the identity repo into both runtime homes, commit
        and push anything new under skills/, then report to the control plane.
        With --from, first copy a skill directory (or a single SKILL.md /
        <name>.md file) into the identity repo at skills/<name>/SKILL.md.
        Idempotent — safe to re-run.

  kyber-skills list [--json]
        Print this agent's skills and any problems found.

  kyber-skills report
        Scan and push the report to the control plane. Fail-soft.

Common flags:
  --repo-dir PATH   identity repo clone (default: $HOME/dev/<KYBER_IDENTITY_REPO name>)
  --home PATH       agent home holding .claude/skills and .codex/skills (default: $HOME)
`)
}

// paths resolves the identity repo and home directory. Both may be passed
// explicitly, which is how the identity-repo sync script calls this: that
// script runs under nsenter as root, where the pod's environment is NOT
// visible, so anything read from the env there would silently be wrong.
type paths struct {
	repoDir     string
	homeDir     string
	platformDir string
}

func addPathFlags(fs *flag.FlagSet, p *paths) {
	fs.StringVar(&p.repoDir, "repo-dir", "", "identity repo clone")
	fs.StringVar(&p.homeDir, "home", "", "agent home directory")
	fs.StringVar(&p.platformDir, "platform-dir", "", "image-bundled skills directory")
}

// resolve fills anything the caller left blank from the environment.
//
// An absent identity repo is NOT an error. Both start scripts pass
// `--repo-dir ""` for agents configured without one, and such an agent still
// has the runtime image's bundled skills plus anything hand-written into a
// runtime home. Failing here would leave exactly those agents showing "no
// report yet" forever — the blind spot the feature exists to remove. Commands
// that genuinely need a repo (install) check for one themselves.
func (p *paths) resolve() error {
	if p.homeDir == "" {
		p.homeDir = os.Getenv("HOME")
	}
	if p.homeDir == "" {
		p.homeDir = "/home/kyber"
	}
	if p.platformDir == "" {
		p.platformDir = os.Getenv("KYBER_PLATFORM_SKILLS_DIR")
	}
	if p.repoDir == "" {
		if slug := os.Getenv("KYBER_IDENTITY_REPO"); slug != "" {
			p.repoDir = filepath.Join(p.homeDir, "dev", slug[strings.LastIndex(slug, "/")+1:])
		}
	}
	return nil
}

// requireRepo is the check for commands that write to the identity repo. There
// is nowhere else a skill can be saved and survive a reprovision.
func (p *paths) requireRepo() error {
	if p.repoDir == "" {
		return errors.New("no identity repo: pass --repo-dir or set KYBER_IDENTITY_REPO")
	}
	return nil
}

func runReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	var p paths
	addPathFlags(fs, &p)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := p.resolve(); err != nil {
		fmt.Fprintf(os.Stderr, "kyber-skills: %v\n", err)
		return 0
	}
	rep, err := skillscan.Scan(skillscan.Options{RepoDir: p.repoDir, HomeDir: p.homeDir, PlatformDir: p.platformDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "kyber-skills: scan failed: %v\n", err)
		return 0
	}
	// The status sidecar is a sibling container, so at boot this can run
	// before it has bound its listener. Retry briefly rather than lose the
	// report until the agent's next restart, which could be days away.
	if err := postReportWithRetry(rep); err != nil {
		// Fail-soft on purpose: this runs inside the boot path, and a
		// control plane that is briefly unreachable must never stop an
		// agent from starting. Say so loudly in the log instead — silence
		// on failure is how a dead reporter goes unnoticed for releases.
		fmt.Fprintf(os.Stderr, "kyber-skills: report NOT delivered: %v\n", err)
		return 0
	}
	fmt.Printf("kyber-skills: reported %d skill(s), %d broken, %d other issue(s)\n",
		len(rep.Skills), countBroken(rep), len(rep.Issues))
	return 0
}

func runList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var p paths
	asJSON := fs.Bool("json", false, "print the raw report as JSON")
	addPathFlags(fs, &p)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := p.resolve(); err != nil {
		fmt.Fprintf(os.Stderr, "kyber-skills: %v\n", err)
		return 1
	}
	rep, err := skillscan.Scan(skillscan.Options{RepoDir: p.repoDir, HomeDir: p.homeDir, PlatformDir: p.platformDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "kyber-skills: scan failed: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return 0
	}
	printReport(os.Stdout, rep, p.repoDir)
	return 0
}

func runInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	var p paths
	from := fs.String("from", "", "skill directory or SKILL.md to import into the identity repo")
	name := fs.String("name", "", "skill name (default: derived from --from)")
	noPush := fs.Bool("no-push", false, "link and report, but do not commit or push")
	addPathFlags(fs, &p)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := p.resolve(); err != nil {
		fmt.Fprintf(os.Stderr, "kyber-skills: %v\n", err)
		return 1
	}
	if err := p.requireRepo(); err != nil {
		fmt.Fprintf(os.Stderr, "kyber-skills: %v\n", err)
		return 1
	}
	if _, err := os.Stat(filepath.Join(p.repoDir, ".git")); err != nil {
		fmt.Fprintf(os.Stderr, "kyber-skills: no identity repo at %s — a skill written anywhere else is lost at the next reprovision\n", p.repoDir)
		return 1
	}

	if *from != "" {
		dest, err := importSkill(p.repoDir, *from, *name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kyber-skills: import failed: %v\n", err)
			return 1
		}
		fmt.Printf("kyber-skills: imported %s\n", dest)
	}

	linked, err := linkAll(p.repoDir, p.homeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kyber-skills: linking failed: %v\n", err)
		return 1
	}
	fmt.Printf("kyber-skills: linked %d skill(s) into %s\n", linked, strings.Join(runtimeSkillDirs, " and "))

	if !*noPush {
		if err := commitAndPush(p.repoDir); err != nil {
			// The skill is live in this pod but not committed anywhere, so
			// it dies at the next reprovision. That is a real failure and
			// the agent needs to know to fix it, not a warning to bury.
			fmt.Fprintf(os.Stderr, "kyber-skills: skills are linked but NOT pushed: %v\n", err)
			fmt.Fprintf(os.Stderr, "kyber-skills: they will be lost when this pod is reprovisioned — push %s by hand\n", p.repoDir)
			return 1
		}
	}

	rep, err := skillscan.Scan(skillscan.Options{RepoDir: p.repoDir, HomeDir: p.homeDir, PlatformDir: p.platformDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "kyber-skills: scan failed: %v\n", err)
		return 1
	}
	if err := postReport(rep); err != nil {
		fmt.Fprintf(os.Stderr, "kyber-skills: report NOT delivered (skills are installed regardless): %v\n", err)
	}
	printReport(os.Stdout, rep, p.repoDir)
	return 0
}

// importSkill copies src into <repo>/skills/<name>/ so a skill the agent wrote
// or downloaded elsewhere lands in the one place the platform can see it.
//
// src may be a directory (copied whole, preserving bundled references/) or a
// single markdown file (installed as SKILL.md).
func importSkill(repoDir, src, name string) (string, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", src, err)
	}
	if name == "" {
		base := filepath.Base(strings.TrimRight(src, string(filepath.Separator)))
		if !info.IsDir() {
			base = strings.TrimSuffix(base, filepath.Ext(base))
			if strings.EqualFold(base, "SKILL") {
				return "", errors.New("cannot derive a skill name from a bare SKILL.md — pass --name")
			}
		}
		name = base
	}
	if err := validSkillName(name); err != nil {
		return "", err
	}
	dest := filepath.Join(repoDir, "skills", name)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	if info.IsDir() {
		if err := copyTree(src, dest); err != nil {
			return "", err
		}
	} else {
		if err := copyFile(src, filepath.Join(dest, "SKILL.md")); err != nil {
			return "", err
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		return "", fmt.Errorf("%s has no SKILL.md — a skill package needs one or no runtime will load it", dest)
	}
	return filepath.Join("skills", name), nil
}

// validSkillName keeps an imported name to something usable as a directory and
// as a slash-command: it becomes both.
func validSkillName(name string) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("invalid skill name %q: must be 1-64 characters", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("invalid skill name %q: use lowercase letters, digits, '-' and '_'", name)
		}
	}
	return nil
}

// linkAll applies the same rules as the boot/sync linker in
// images/shared/kyber-identity-repo.sh: identity skills first, then each
// vendored package, each replacing any existing link of the same name. Running
// it here means a skill written mid-session is loadable immediately instead of
// at the next boot — which is what made "the agent saved a skill" and "the
// agent has a skill" two different things.
func linkAll(repoDir, homeDir string) (int, error) {
	for _, rel := range runtimeSkillDirs {
		if err := os.MkdirAll(filepath.Join(homeDir, rel), 0o755); err != nil {
			return 0, err
		}
	}
	sources := []string{filepath.Join(repoDir, "skills")}
	if vendors, err := os.ReadDir(filepath.Join(repoDir, "vendor")); err == nil {
		var names []string
		for _, v := range vendors {
			if v.IsDir() {
				names = append(names, v.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			sources = append(sources, filepath.Join(repoDir, "vendor", n, "skills"))
		}
	}

	var count int
	for _, src := range sources {
		entries, err := os.ReadDir(src)
		if err != nil {
			continue
		}
		for _, e := range entries {
			dir := filepath.Join(src, e.Name())
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
				continue
			}
			for _, rel := range runtimeSkillDirs {
				dst := filepath.Join(homeDir, rel, e.Name())
				if err := os.RemoveAll(dst); err != nil {
					return count, err
				}
				if err := os.Symlink(dir, dst); err != nil {
					return count, err
				}
			}
			count++
		}
	}
	return count, nil
}

// commitAndPush persists everything under skills/ to the identity repo. Git
// authentication is already configured in the pod (the Kyber App credential
// helper), so this needs no token of its own.
func commitAndPush(repoDir string) error {
	if out, err := git(repoDir, "add", "--", "skills"); err != nil {
		return fmt.Errorf("git add: %v: %s", err, out)
	}
	// --quiet + --exit-code: 0 means nothing staged, so there is nothing to
	// commit and pushing again would be a no-op. Not an error.
	if _, err := git(repoDir, "diff", "--cached", "--quiet"); err == nil {
		fmt.Println("kyber-skills: no skill changes to commit")
		return nil
	}
	if out, err := git(repoDir, "commit", "-m", "skills: install via kyber-skills"); err != nil {
		return fmt.Errorf("git commit: %v: %s", err, out)
	}
	if out, err := git(repoDir, "push"); err != nil {
		return fmt.Errorf("git push: %v: %s", err, out)
	}
	fmt.Println("kyber-skills: committed and pushed skills/ to the identity repo")
	return nil
}

func git(repoDir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoDir}, args...)...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return strings.TrimSpace(buf.String()), err
}

// postReport delivers the report through the status sidecar's localhost
// forwarder, which adds the agent name and auth on the way to the control
// plane (the same path token-usage and runtime-catalog already use).
func postReport(rep *skillscan.Report) error {
	base := os.Getenv("KYBER_SIDECAR_URL")
	if base == "" {
		base = defaultSidecarURL
	}
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), postTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/skills", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &responseError{status: resp.Status, body: strings.TrimSpace(string(msg))}
	}
	return nil
}

// reportAttempts and reportRetryDelay bound the boot-time wait: about 12
// seconds total, which comfortably covers a sidecar that is still binding
// without stalling a boot when the sidecar is genuinely absent.
const (
	reportAttempts   = 5
	reportRetryDelay = 3 * time.Second
)

// postReportWithRetry retries only what retrying can fix. A refused connection
// means the sidecar has not bound its listener yet, which resolves on its own;
// an actual HTTP response — a rejected body, or an install with no skill store
// — will say exactly the same thing five times, so it is returned immediately.
func postReportWithRetry(rep *skillscan.Report) error {
	var err error
	for attempt := 1; attempt <= reportAttempts; attempt++ {
		err = postReport(rep)
		if err == nil {
			return nil
		}
		var httpErr *responseError
		if errors.As(err, &httpErr) {
			return err
		}
		if attempt < reportAttempts {
			fmt.Fprintf(os.Stderr, "kyber-skills: report attempt %d/%d could not reach the sidecar (%v) — retrying in %s\n",
				attempt, reportAttempts, err, reportRetryDelay)
			time.Sleep(reportRetryDelay)
		}
	}
	return err
}

// responseError is a non-2xx answer from the sidecar or control plane, as
// opposed to a failure to reach them at all.
type responseError struct {
	status string
	body   string
}

func (e *responseError) Error() string {
	if e.body == "" {
		return "sidecar returned " + e.status
	}
	return "sidecar returned " + e.status + ": " + e.body
}

func countBroken(rep *skillscan.Report) int {
	var n int
	for _, s := range rep.Skills {
		if s.Broken() {
			n++
		}
	}
	return n
}

// printReport writes the human-readable view an agent reads when it wants to
// know what it can actually do.
func printReport(w io.Writer, rep *skillscan.Report, repoDir string) {
	if repoDir == "" {
		fmt.Fprint(w, "\nSkills (no identity repo — only what the runtime image provides)\n")
	} else {
		fmt.Fprintf(w, "\nSkills in %s\n", repoDir)
	}
	if len(rep.Skills) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, s := range rep.Skills {
		origin := "own"
		if s.Source == skillscan.SourceVendor {
			origin = "vendor:" + s.SourcePackage
		}
		status := "ok"
		switch {
		case s.Broken():
			status = "BROKEN"
		case !s.Healthy():
			status = "warn"
		}
		fmt.Fprintf(w, "  [%-7s] %-28s %-24s %s\n", status, s.Name, "("+origin+")", truncate(s.Description, 60))
		for _, iss := range s.Issues {
			fmt.Fprintf(w, "            └─ [%s] %s: %s\n", iss.Severity, iss.Code, iss.Detail)
		}
	}
	if len(rep.Issues) > 0 {
		fmt.Fprintln(w, "\nOther problems:")
		for _, iss := range rep.Issues {
			fmt.Fprintf(w, "  - [%s] %s: %s\n", iss.Severity, iss.Code, iss.Detail)
		}
	}
	fmt.Fprintln(w)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// copyTree copies src into dst recursively. Symlinks are skipped rather than
// followed: a skill package that reaches outside itself would not survive being
// committed to the identity repo anyway.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return nil
		case info.IsDir():
			return os.MkdirAll(target, 0o755)
		default:
			return copyFile(path, target)
		}
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
