#!/usr/bin/env bash
# Print the CHANGELOG.md section body for a given version, for use as a
# GitHub Release body. Usage: release-notes.sh <version> [changelog-path]
#
#   1.18.0        -> body between "## [1.18.0]" and the next "## [" (heading stripped)
#   1.18.1-rc.0   -> a placeholder line (pre-release: no section is expected)
#   99.99.99      -> exit 1 (a stable release MUST have a changelog section)
set -euo pipefail

version="${1:?usage: release-notes.sh <version> [changelog-path]}"
changelog="${2:-CHANGELOG.md}"

# Extract the section body (lines after the matching heading, up to the next
# "## [" heading), then trim leading/trailing blank lines. Pure awk (no sed)
# to stay portable between BSD awk/sed (macOS, local dev) and GNU (CI).
body="$(
  awk -v ver="$version" '
    index($0, "## [" ver "]") == 1 { grab=1; next }
    grab && index($0, "## [") == 1 { grab=0 }
    grab { print }
  ' "$changelog" | awk '
    { lines[NR] = $0; if ($0 !~ /^[[:space:]]*$/) last = NR }
    END {
      first = 1
      while (first <= NR && lines[first] ~ /^[[:space:]]*$/) first++
      for (i = first; i <= last; i++) print lines[i]
    }
  '
)"

if [ -n "$body" ]; then
  printf '%s\n' "$body"
  exit 0
fi

# No section found.
case "$version" in
  *-*)  # pre-release (semver build/prerelease metadata contains a hyphen)
    printf 'Pre-release %s. Not a stable release; see CHANGELOG.md for stable notes.\n' "$version"
    exit 0
    ;;
  *)
    printf 'error: no CHANGELOG section for stable version %s\n' "$version" >&2
    exit 1
    ;;
esac
