#!/usr/bin/env bash
# Reject source files that hide code behind a long run of whitespace.
#
# This is how the GlassWorm payload was carried here: dashboard/vite.config.js line 56
# was 31,305 characters — the real `});` closing defineConfig, then 507 spaces, then
# ~30KB of obfuscated JavaScript. It read as a normal 56-line config in any editor that
# does not wrap, and `vite build` executed it on every build.
#
# Two independent signals, either of which fails:
#   1. content, then 200+ consecutive spaces, then more content, on one line
#   2. the payload's own marker, which every variant seen so far carries
#
# Also flags font files that are actually text — a real .woff2 starts with `wOF2`.
#
# Portable to bash 3 (macOS) as well as the bash 5 on CI runners: no mapfile, and
# a missing-match grep never trips `set -e`.
set -uo pipefail

ROOT="${1:-.}"
status=0

FILES=()
while IFS= read -r -d '' f; do FILES+=("$f"); done < <(find "$ROOT" \
  \( -path '*/node_modules' -o -path '*/.git' -o -path '*/dist' -o -path '*/build' \
     -o -path '*/vendor' -o -path '*/.next' -o -path '*/coverage' \) -prune -o \
  -type f \( -name '*.js' -o -name '*.jsx' -o -name '*.ts' -o -name '*.tsx' \
     -o -name '*.mjs' -o -name '*.cjs' -o -name '*.json' \) -print0 2>/dev/null)

if [ "${#FILES[@]}" -gt 0 ]; then
  # One grep pass, not a loop: content + 200 spaces + content.
  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    echo "HIDDEN-CODE     $hit  (code after 200+ spaces)"; status=1
  done < <(LC_ALL=C grep -lE '[^[:space:]] {200,}[^[:space:]]' "${FILES[@]}" 2>/dev/null || true)

  while IFS= read -r hit; do
    [ -n "$hit" ] || continue
    echo "PAYLOAD-MARKER  $hit  (global.i=\"A8-\")"; status=1
  done < <(LC_ALL=C grep -lF 'global.i="A8-' "${FILES[@]}" 2>/dev/null || true)
fi

# A font that is not a font. Real woff2 magic is `wOF2` (0x774f4632).
while IFS= read -r -d '' f; do
  [ -s "$f" ] || continue
  if [ "$(head -c 4 "$f" | LC_ALL=C tr -d '\0')" != "wOF2" ]; then
    echo "FAKE-FONT       $f  (not wOF2 magic — masqueraded payload?)"; status=1
  fi
done < <(find "$ROOT" \( -path '*/node_modules' -o -path '*/.git' \) -prune -o -type f -name '*.woff2' -print0 2>/dev/null)

if [ "$status" -eq 0 ]; then
  echo "check-hidden-code: clean"
else
  echo; echo "check-hidden-code: FAILED — see above."
  echo "A real font never contains text; a config never needs a 30KB line."
fi
exit "$status"
