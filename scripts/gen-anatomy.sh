#!/usr/bin/env bash
# Generate a per-file anatomy table for the kyber repo. One-shot tool —
# NOT auto-maintained or loaded into agent context by default. See
# docs/adr/0002-file-anatomy-index.md for the rationale (the standing-
# pattern version was evaluated against kyber#173 and rejected).
#
# Use case: ad-hoc snapshot before a large refactor. Run from repo root:
#     ./scripts/gen-anatomy.sh > /tmp/anatomy.md
#
# Heuristic: tokens ≈ chars / 4 (the rough Anthropic-tokenizer estimate).
# "Purpose" = first non-empty doc comment in the file (//, #, or # for
# yaml). Files with no doc comment land with empty purpose — readers
# should treat empty-purpose rows as "go look at the file."

set -euo pipefail
ROOT="${1:-.}"
cd "$ROOT"

extract_purpose() {
    local f="$1"
    case "$f" in
        *.go|*.tsx|*.ts|*.js|*.jsx)
            # First contiguous run of // comments (anywhere near top).
            awk '
                /^[[:space:]]*\/\// {
                    sub(/^[[:space:]]*\/\/[[:space:]]*/, "")
                    if ($0 != "") { print; lines++; if (lines >= 1) exit }
                }
                /^[[:space:]]*[a-zA-Z]/ && lines > 0 { exit }
                NR > 50 { exit }
            ' "$f" | head -c 100
            ;;
        *.sh)
            awk '
                NR==1 && /^#!/ { next }
                /^#/ {
                    sub(/^#[[:space:]]*/, "")
                    if ($0 != "") { print; exit }
                }
                /^[a-zA-Z]/ { exit }
            ' "$f" | head -c 100
            ;;
        *.md|*.MD)
            grep -m 1 '^# ' "$f" 2>/dev/null | sed 's/^# //' | head -c 100
            ;;
    esac
}

echo "| path | tokens | purpose |"
echo "|---|---:|---|"

find pkg cmd images pwa/src 2>/dev/null \
    -type f \
    \( -name '*.go' -o -name '*.tsx' -o -name '*.ts' -o -name '*.sh' \) \
    -not -name '*_test.go' \
    -not -name '*.test.tsx' -not -name '*.test.ts' \
    -not -name '*.spec.ts' -not -name '*.spec.tsx' \
    -not -name 'zz_generated.*' \
    -not -path '*/node_modules/*' \
    -not -path '*/dist/*' \
    -not -path '*/pwa_dist/*' \
    | sort -u | while read -r f; do
        chars=$(wc -c < "$f" 2>/dev/null || echo 0)
        tokens=$(( chars / 4 ))
        purpose=$(extract_purpose "$f" | tr -d '|' | tr '\n' ' ' | sed 's/  */ /g')
        printf "%d|%s|%s\n" "$tokens" "$f" "$purpose"
    done | sort -t'|' -k1,1nr | awk -F'|' '{ printf "| `%s` | %d | %s |\n", $2, $1, $3 }'
