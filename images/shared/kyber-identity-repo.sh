#!/bin/bash
# images/shared/kyber-identity-repo.sh
#
# Clone-or-sync the agent's identity repo and install the git credential helper.
# SOURCED (not executed) by every runtime's start script — it sets REPO_DIR and
# KYBER_SYNC_SCRIPT, which callers use afterwards, so it must run in the
# caller's shell.
#
# Extracted verbatim from images/claude-code/start-claude.sh (kyber#676). It
# lived only there, so the Codex runtime shipped the CONSUMER of the identity
# repo without the producer: start-codex.sh checks for $HOME/dev/<repo>/.git to
# pick its launch dir, that check could never be true, and HK-47 — the first
# prod Codex agent — booted with no identity repo, no memory, and no sync
# script, while status.identityRepo said "Ready" (which only ever meant the
# GitHub repo exists, not that the agent has it).
#
# Deliberately shared rather than copied: this is the code that manages every
# agent's identity and git credentials, which makes it the worst possible place
# for two copies to drift apart.
#
# Contract — inputs (all read from the environment):
#   KYBER_IDENTITY_REPO            owner/name slug; when unset this is a no-op
#   HOME                           defaulted to /home/kyber when unset
#   AGENT_NAME, KYBER_CONTROL_PLANE_INTERNAL_URL, pod-token
#                                  used to mint the Kyber App token
#   KYBER_SYNC_SCRIPT              optional override for the generated script path
# Outputs (left set in the caller's shell):
#   REPO_DIR                       absolute path to the clone
#   KYBER_SYNC_SCRIPT              path to the generated sync script
#
# Never exits non-zero on a failed identity setup: a broken identity repo must
# not stop the agent from booting.


# ---- Identity repo ----
# When KYBER_IDENTITY_REPO is set, clone-or-sync the agent's identity repo into
# ~/dev/<name>. The identity repo (reads AND writes) is managed EXCLUSIVELY by
# the install's Kyber Platform GitHub App (kyber#508 Stage 3/4): the credential
# helper installed below mints a short-lived, repo-scoped token from the control
# plane for the identity repo, with NO PAT fallback — if the App flow fails, git
# fails loudly rather than silently masking a broken path with the broad PAT.
# The generic PAT ($GH_TOKEN / $USER_GITHUB_TOKEN) is used only for the agent's
# OTHER repos. No token is baked into ~/.gitconfig; everything is read fresh on
# every git call. (Supersedes the kyber#509 PAT-only cutover.)
if [ -n "${KYBER_IDENTITY_REPO:-}" ]; then
    # HOME may be unset when start-claude runs under entrypoint.sh; mirror the
    # token-reporter block's fallback so the clone lands where future Chewie-
    # style identity layouts expect it.
    export HOME="${HOME:-/home/kyber}"
    REPO_SLUG="${KYBER_IDENTITY_REPO}"

    # Identity-repo git (clone/fetch/push) is authenticated by the credential
    # helper installed below: the agent's OWN identity repo uses a Kyber Platform
    # App token (no PAT required — so a PAT-less agent, e.g. a freshly scaffolded
    # one, still clones), and every other repo uses the generic PAT. So the
    # identity-repo setup is NOT gated on a PAT being present.

    # Defense-in-depth: the controller is the authority on this value, but the
    # slug flows into a URL passed to `git clone` and into a heredoc written to
    # disk — reject anything that isn't a plain owner/name slug before using it
    # so a misconfigured CRD can't become shell injection.
    if ! printf '%s' "$REPO_SLUG" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$'; then
        echo "[kyber] WARNING: KYBER_IDENTITY_REPO='$REPO_SLUG' is not a valid owner/name slug — skipping identity repo setup"
        REPO_SLUG=""
    fi

    REPO_NAME="${REPO_SLUG##*/}"
    REPO_DIR="${HOME}/dev/${REPO_NAME}"
    HELPER_DIR="${HOME}/.local/bin"
    HELPER_PATH="${HELPER_DIR}/git-credential-kyber-github"

    if [ -z "$REPO_SLUG" ]; then
        : # already warned above
    else
        mkdir -p "$HELPER_DIR"
        # Re-write on every boot so fixes to the helper body ship without extra
        # migration. The helper bakes in NO operator-controlled string and reads
        # everything fresh from the environment on every call (a quoted heredoc,
        # so nothing is interpolated at write time).
        #
        # Behaviour (kyber#508 Stage 3/4): for the agent's OWN identity repo the
        # helper mints a short-lived token via the Kyber Platform GitHub App
        # (through the control plane) and emits it — with NO PAT fallback. If the
        # App flow fails, the helper emits nothing so git fails LOUDLY: a broken
        # identity-repo credential path must surface, never be masked by the PAT.
        # For every OTHER repo (maintainer work, dev repos) it emits the generic
        # PAT. Tokens are cached (mode 600) until shortly before expiry so a busy
        # repo doesn't mint one per git call.
        cat > "$HELPER_PATH" <<'HELPER_EOF'
#!/bin/sh
# git credential helper (kyber#508 Stage 3/4).
#   identity repo -> short-lived Kyber-App token via the control plane, cached;
#                    NO PAT fallback — emit nothing on failure so git fails loud.
#   any other repo -> generic PAT.
[ "$1" = "get" ] || exit 0

# git passes the request context as key=value lines on stdin (blank-line
# terminated). It only includes path= when credential.useHttpPath=true (set at
# boot); without it we cannot tell which repo, so we treat it as non-identity.
reqpath=""
while IFS= read -r line; do
    case "$line" in
        path=*) reqpath="${line#path=}" ;;
        "") break ;;
    esac
done

# Every unhappy path here ends with git receiving NO credential, and git then
# retries anonymously. Against a PRIVATE repo GitHub answers 404 — so the
# operator-visible error is "Repository not found", which reads as "your repo is
# missing or the App lacks access" when the truth is "this helper produced
# nothing". hk-47 lost real time to exactly that on 2026-08-06: he was told the
# repo did not exist, and went off to audit repo existence and App permissions
# while both were demonstrably fine (the platform's own boot sync had pulled the
# same repo seconds earlier). So: never exit silently. Name the reason on stderr,
# which git forwards to the terminal, and say what the misleading error will be.
# printf, not echo: this interpolates reqpath, which comes from git, and some
# /bin/sh echoes (dash) interpret backslash escapes in their argument.
say() { printf 'kyber: %s\n' "$1" >&2; }
note_404() {
    say "git will now retry ANONYMOUSLY. For a private repo GitHub answers 404, so you are about to see 'Repository not found' — that is a symptom of the credential failure above, NOT proof the repo is missing or the App lacks access."
}

emit_pat() {
    t="${GH_TOKEN:-${USER_GITHUB_TOKEN:-}}"
    if [ -z "$t" ]; then
        say "no credential available for '${reqpath:-<no path>}': not this agent's identity repo (or unrecognised as such), and neither GH_TOKEN nor USER_GITHUB_TOKEN is set."
        note_404
        exit 0
    fi
    printf 'username=x-access-token\npassword=%s\n' "$t"
    exit 0
}

# Only the agent's own identity repo uses the Kyber App. path is "owner/repo.git"
# or "owner/repo"; KYBER_IDENTITY_REPO is "owner/repo".
want="${KYBER_IDENTITY_REPO:-}"
reqslug="${reqpath%.git}"
if [ -z "$want" ]; then
    # The helper cannot recognise the identity repo at all, so even the agent's
    # OWN repo falls through to the PAT branch.
    say "KYBER_IDENTITY_REPO is unset or empty — cannot identify this agent's own identity repo, so every request falls back to the generic PAT."
    emit_pat
fi
if [ -z "$reqpath" ]; then
    # useHttpPath off => git sends no path=, so the identity repo is
    # indistinguishable from any other and silently takes the PAT branch.
    say "git sent no repo path — credential.useHttpPath is not enabled, so the identity repo cannot be told apart from any other and will not use the Kyber App token. Re-run boot, or: git config --global --replace-all credential.https://github.com.useHttpPath true"
    emit_pat
fi
if [ "$reqslug" != "$want" ]; then
    emit_pat
fi

# --- Identity repo: Kyber Platform App token ONLY. No fallback: fail loud. ---
emit_tok() { printf 'username=x-access-token\npassword=%s\n' "$1"; exit 0; }
fail() {
    say "identity-repo credential via Kyber Platform App failed: $1"
    note_404
    exit 0
}

cache="${HOME}/.cache/kyber/identity-token"
now="$(date +%s 2>/dev/null || echo 0)"

# Reuse a cached token while >120s from expiry. Format: "<exp_epoch> <token>".
if [ "$now" -gt 0 ] && [ -r "$cache" ]; then
    cexp="$(cut -d' ' -f1 "$cache" 2>/dev/null)"
    ctok="$(cut -d' ' -f2- "$cache" 2>/dev/null)"
    case "$cexp" in
        '' | *[!0-9]*) : ;;
        *) [ -n "$ctok" ] && [ "$cexp" -gt "$((now + 120))" ] && emit_tok "$ctok" ;;
    esac
fi

# Mint fresh from the control plane, authenticating with the pod-token.
podtok_path="${KYBER_POD_TOKEN_PATH:-/var/run/secrets/kyber/pod-token}"
[ -r "$podtok_path" ] || fail "pod-token not readable"
podtok="$(cat "$podtok_path" 2>/dev/null)"
[ -n "$podtok" ] || fail "empty pod-token"
[ -n "${KYBER_CONTROL_PLANE_INTERNAL_URL:-}" ] || fail "no control-plane URL"
[ -n "${AGENT_NAME:-}" ] || fail "no agent name"
command -v curl >/dev/null 2>&1 || fail "curl unavailable"

resp="$(curl -fsS --max-time 10 -H "Authorization: Bearer ${podtok}" \
    "${KYBER_CONTROL_PLANE_INTERNAL_URL}/internal/agents/${AGENT_NAME}/identity-repo-token" 2>/dev/null)" \
    || fail "control-plane request failed"
tok="$(printf '%s' "$resp" | sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
[ -n "$tok" ] || fail "no token in control-plane response"

# Cache best-effort with the token's expiry (epoch), mode 600.
expat="$(printf '%s' "$resp" | sed -n 's/.*"expires_at"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
expepoch="$(date -d "$expat" +%s 2>/dev/null || echo 0)"
case "$expepoch" in
    '' | *[!0-9]* | 0) : ;;
    *) if mkdir -p "$(dirname "$cache")" 2>/dev/null; then
           ( umask 077; printf '%s %s\n' "$expepoch" "$tok" > "$cache" 2>/dev/null ) || :
       fi ;;
esac

emit_tok "$tok"
HELPER_EOF
        chmod 700 "$HELPER_PATH"

        # --replace-all (not a bare set): the persisted ~/.gitconfig on the PVC
        # may already hold MULTIPLE values for this key (e.g. left by an earlier
        # boot revision that used `gh auth setup-git`, which writes an empty
        # `helper =` reset + the gh helper). credential.helper is a multi-valued
        # key, so a bare single-value set errors with "cannot overwrite multiple
        # values with a single value" — which under `set -euo pipefail` (line 2)
        # crashes boot and wedges the agent Failed with no auto-recovery (the bad
        # config is persisted). --replace-all collapses any 0/1/N pre-existing
        # values to exactly this one, so boot is idempotent across restarts. Keep
        # this idempotent; do NOT regress to a bare `git config` set. See kyber#418.
        git config --global --replace-all "credential.https://github.com.helper" "$HELPER_PATH"
        # Load-bearing for kyber#508 Stage 3/4: git only passes `path=owner/repo`
        # to the credential helper when useHttpPath is on. Without it the helper
        # can't tell which repo git wants and would treat the identity repo as
        # "other" → emit the PAT, silently masking the App flow (exactly what the
        # no-fallback design forbids). --replace-all for the same multi-valued-key
        # idempotency reason as the helper key above (kyber#418).
        git config --global --replace-all "credential.https://github.com.useHttpPath" "true"
        git config --global user.name "${AGENT_NAME:-kyber-agent}"
        git config --global user.email "${AGENT_NAME:-kyber-agent}@agents.kyber.io"

        mkdir -p "$(dirname "$REPO_DIR")"
        if [ ! -d "$REPO_DIR/.git" ]; then
            echo "[kyber] cloning identity repo $REPO_SLUG into $REPO_DIR"
            git clone --quiet "https://github.com/${REPO_SLUG}.git" "$REPO_DIR" \
                || echo "[kyber] WARNING: identity-repo clone failed (continuing without it). If auth failed, the Kyber Platform GitHub App flow is broken for this agent — check the kyber-github-app Secret + control plane; it is NOT silently PAT-backed."
        fi

        # Generate the identity-repo SYNC helper (kyber#596) and run it at boot.
        # The SAME script is invoked by last-claude-launch.sh on a restart-session
        # (below), so a session restart — and the nightly falcon reset that calls
        # it — picks up new skills/contract, not just clears context.
        #
        # It: (1) checks out the CANONICAL branch (kyber#542 — agents end tasks on
        # feature branches), (2) does a DIVERGENCE-TOLERANT authenticated merge
        # (git merge, NOT pull --ff-only) so a clone carrying unpushed local memory
        # commits still lands origin's changes WITHOUT losing memory (kyber#561 —
        # ff-only silently refused and the pod ran stale code), and (3) re-links
        # skills (kyber#323) so newly-merged skills become visible. The fetch
        # authenticates with a short-lived Kyber Platform App token minted FRESH
        # at call time from the control plane (NO PAT — the identity repo is
        # App-managed); the control-plane URL + agent name come from the env at
        # boot, or PID1's environ when run under nsenter from a restart-session,
        # so no secret is persisted into the script. Git runs as the repo-owner
        # (kyber) regardless of caller.
        # Fail-soft throughout: a sync error logs and continues, never crashes
        # boot or the restart. All substitutions are ||-guarded (kyber#548).
        #
        # Where the generated script lands: /persist by default, because it must
        # survive pod recreation and the restart-session path below re-executes it
        # by path. Fall back to $HOME when /persist is absent or unwritable (a CI
        # runner, a dev box, a runtime without the overlay) — previously the
        # redirect just failed there and the whole sync silently didn't happen,
        # which is also why the boot-sync tests could never run outside a pod.
        if [ -z "${KYBER_SYNC_SCRIPT:-}" ]; then
            if [ -d /persist ] && [ -w /persist ]; then
                KYBER_SYNC_SCRIPT=/persist/kyber-sync-identity.sh
            else
                KYBER_SYNC_SCRIPT="${HOME:-/home/kyber}/.kyber-sync-identity.sh"
                echo "[kyber] sync: /persist unavailable — generating sync script at $KYBER_SYNC_SCRIPT"
            fi
        fi
        {
            printf '#!/bin/bash\n# Generated by start-claude.sh (kyber#596) — do not edit.\nset -u\n'
            printf 'REPO_DIR=%q\nREPO_SLUG=%q\nHOME_DIR=%q\n' "$REPO_DIR" "$REPO_SLUG" "$HOME"
            cat <<'SYNC_BODY'
[ -d "$REPO_DIR/.git" ] || { echo "[kyber] sync: no repo at $REPO_DIR — skip"; exit 0; }
# git as the repo owner (kyber) regardless of caller (root under nsenter on a restart).
RUN() { if [ "$(id -u)" = "0" ]; then sudo -u kyber "$@"; else "$@"; fi; }
# The identity repo is App-managed: obtain a short-lived, repo-scoped token from
# the control plane via the Kyber Platform GitHub App (the same path the git
# credential helper uses). NO PAT — a read of the identity repo must not ride the
# broad PAT. Resolve the control-plane URL + agent name from the env (boot) or
# PID1's environ (root, under nsenter from a restart-session); the pod-token
# authenticates the mint request.
# Readable-guard first: the shell reports a failed input redirect itself, so an
# unreadable /proc/1/environ (running as non-root, or outside a pod) printed a bare
# "Permission denied" line that looked like a sync failure. The env-var path below
# is the normal one at boot; PID1 is only the nsenter/restart-session fallback.
pid1env() { [ -r /proc/1/environ ] || return 0; tr '\0' '\n' < /proc/1/environ 2>/dev/null | sed -n "s/^$1=//p" | head -1; }
CP_URL="${KYBER_CONTROL_PLANE_INTERNAL_URL:-$(pid1env KYBER_CONTROL_PLANE_INTERNAL_URL)}"
AGENT="${AGENT_NAME:-$(pid1env AGENT_NAME)}"
POD_TOKEN=$(cat "${KYBER_POD_TOKEN_PATH:-/var/run/secrets/kyber/pod-token}" 2>/dev/null)
APP_TOK=""
if [ -n "$POD_TOKEN" ] && [ -n "$CP_URL" ] && [ -n "$AGENT" ] && command -v curl >/dev/null 2>&1; then
    APP_TOK=$(curl -fsS --max-time 10 -H "Authorization: Bearer ${POD_TOKEN}" \
        "${CP_URL}/internal/agents/${AGENT}/identity-repo-token" 2>/dev/null \
        | sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
fi
DEFAULT_BRANCH=$(RUN git -C "$REPO_DIR" symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null | sed 's#^origin/##') || true
DEFAULT_BRANCH=${DEFAULT_BRANCH:-main}
# Say so when the agent had been left parked on a feature branch. kyber#542 was
# exactly this failure — an agent ending a task on a branch and then running stale
# code for days — and silently correcting it hides how often it happens. (The
# message was dropped in the kyber#596 sync rewrite and restored here.)
CUR_BRANCH=$(RUN git -C "$REPO_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null) || true
if [ -n "${CUR_BRANCH:-}" ] && [ "$CUR_BRANCH" != "HEAD" ] && [ "$CUR_BRANCH" != "$DEFAULT_BRANCH" ]; then
    echo "[kyber] sync: WARNING repo was on '$CUR_BRANCH' — switching to default branch '$DEFAULT_BRANCH' (kyber#542)"
fi
RUN git -C "$REPO_DIR" checkout --quiet "$DEFAULT_BRANCH" 2>/dev/null \
    || echo "[kyber] sync: checkout $DEFAULT_BRANCH failed (continuing on current branch)"
if [ -n "$APP_TOK" ]; then
    if RUN git -C "$REPO_DIR" fetch --quiet "https://x-access-token:${APP_TOK}@github.com/${REPO_SLUG}.git" "$DEFAULT_BRANCH" 2>/dev/null; then
        if ! RUN git -C "$REPO_DIR" merge --no-edit FETCH_HEAD 2>/dev/null; then
            RUN git -C "$REPO_DIR" merge --abort 2>/dev/null || true
            echo "[kyber] sync: merge conflict on $DEFAULT_BRANCH — kept existing tree (manual reconcile needed)"
        else
            echo "[kyber] sync: $REPO_SLUG @ $DEFAULT_BRANCH synced"
        fi
    else
        echo "[kyber] sync: fetch failed (continuing on existing tree)"
    fi
else
    echo "[kyber] sync: could not obtain Kyber Platform App token — skipping fetch (identity repo is App-managed, NOT PAT-backed; check the kyber-github-app Secret + control plane)"
fi
# Persist curated memory to the identity repo (kyber#625). Claude Code's
# file-based memory defaults to ~/.claude/projects/<cwd-slug>/memory, which is
# NOT inside the identity repo — so /compact-memory edits and saved memories were
# never committed or pushed (they lived only on the pod's disk, one reprovision
# from loss, and couldn't be audited from git). Symlink that native dir onto the
# repo's memory/ so the auto-memory Stop hook + /compact-memory persist it,
# exactly like Dave (whose memory is already in-repo). Migrate any pre-existing
# native memory into the repo first (no-clobber) so nothing on-pod is lost. Runs
# AFTER the merge above so migrated (untracked) files can't trip a merge; the
# auto-memory hook commits + pushes them on the next Stop. Idempotent: once the
# native path is a symlink, migration is skipped and the link is just refreshed.
MEM_SLUG=$(printf '%s' "$REPO_DIR" | sed 's#/#-#g')
NATIVE_MEM="$HOME_DIR/.claude/projects/$MEM_SLUG/memory"
REPO_MEM="$REPO_DIR/memory"
RUN mkdir -p "$REPO_MEM"
if [ -e "$NATIVE_MEM" ] && [ ! -L "$NATIVE_MEM" ]; then
    RUN cp -an "$NATIVE_MEM/." "$REPO_MEM/" 2>/dev/null || true
    RUN rm -rf "$NATIVE_MEM"
    echo "[kyber] sync: migrated on-pod native memory into $REPO_MEM"
fi
RUN mkdir -p "$(dirname "$NATIVE_MEM")"
RUN ln -sfn "$REPO_MEM" "$NATIVE_MEM"
RUN chown -h kyber:kyber "$NATIVE_MEM" 2>/dev/null || true
echo "[kyber] sync: memory wired → $REPO_MEM"
mkdir -p "$HOME_DIR/.claude/skills" "$HOME_DIR/.codex/skills"
for skills_src in "$REPO_DIR/skills" "$REPO_DIR/vendor"/*/skills; do
    [ -d "$skills_src" ] || continue
    # Primary layout (identity-repo convention, see kyber-agent-template README):
    # skills/<name>/SKILL.md, optionally with bundled references/ or assets.
    # Symlink the whole directory so SKILL.md AND its siblings resolve. Both
    # supported runtimes use the same skill package shape, under their own
    # homes: ~/.claude/skills and ~/.codex/skills.
    for d in "$skills_src"/*/; do
        [ -f "${d}SKILL.md" ] || continue
        name=$(basename "$d")
        for runtime_skills in "$HOME_DIR/.claude/skills" "$HOME_DIR/.codex/skills"; do
            rm -rf "$runtime_skills/$name"
            ln -sf "${d%/}" "$runtime_skills/$name"
        done
    done
    # Compat layout: flat skills/<name>.md (link just the file as SKILL.md).
    # A README in skills/ is documentation, not a skill — linking it would
    # publish a junk "README" command to both runtimes.
    for f in "$skills_src"/*.md; do
        [ -f "$f" ] || continue
        name=$(basename "$f" .md)
        case "$name" in README | readme | index) continue ;; esac
        for runtime_skills in "$HOME_DIR/.claude/skills" "$HOME_DIR/.codex/skills"; do
            rm -rf "$runtime_skills/$name"
            mkdir -p "$runtime_skills/$name"
            ln -sf "$f" "$runtime_skills/$name/SKILL.md"
        done
    done
done
chown -R kyber:kyber "$HOME_DIR/.claude/skills" "$HOME_DIR/.codex/skills" 2>/dev/null || true
echo "[kyber] sync: skills re-linked for Claude Code and Codex"
# Push the resulting inventory to the control plane so the Kyber UI shows what
# this agent can ACTUALLY invoke, not what its repo happens to contain — the two
# were silently different for the whole of kyber#691. Paths are passed
# explicitly because this script also runs under nsenter as root, where the
# pod's environment is not visible and anything read from it would be wrong.
# Fail-soft: a report that does not land must never break a boot or a restart.
if command -v kyber-skills >/dev/null 2>&1; then
    # --once: a converge loop is already running from boot, so this is just an
    # immediate refresh for the restart-session path, which is exactly when
    # newly-merged skills arrive.
    RUN kyber-skills report --once --repo-dir "$REPO_DIR" --home "$HOME_DIR" \
        || echo "[kyber] sync: skill report failed (continuing)"
fi
exit 0
SYNC_BODY
        } > "$KYBER_SYNC_SCRIPT"
        chmod 0755 "$KYBER_SYNC_SCRIPT"
        /bin/bash "$KYBER_SYNC_SCRIPT" || echo "[kyber] WARNING: identity sync failed (continuing)"
    fi
fi
