#!/usr/bin/env bash
# Print the CHANGELOG.md section for one version, for use as GitHub release
# notes. The notes an operator reads at a tag and the notes in the repository
# are then the same words, which is the only way they stay true.
#
#   scripts/release-notes.sh v0.1.0            > notes.md
#   scripts/release-notes.sh 0.1.0 CHANGELOG.md
#   scripts/release-notes.sh latest            the newest section, for a rehearsal
#
# A version with no section is an error rather than an empty file: a release
# that nobody wrote down is a release nobody can upgrade to safely.
set -euo pipefail

version=${1:?usage: release-notes.sh <version> [changelog]}
changelog=${2:-CHANGELOG.md}
version=${version#v}

if [ ! -f "$changelog" ]; then
	echo "release-notes: no $changelog here" >&2
	exit 1
fi

# "latest" is for rehearsing before the tag exists: take the newest version
# section, which is the one the next tag will publish.
if [ "$version" = "latest" ]; then
	version=$(grep -m1 -oE '^## \[[0-9]+\.[0-9]+\.[0-9]+\]' "$changelog" | tr -d '#[] ')
	if [ -z "$version" ]; then
		echo "release-notes: $changelog has no released version to take as the latest" >&2
		exit 1
	fi
fi

notes=$(awk -v want="$version" '
	# Section headings look like "## [0.1.0] — 2026-09-13" or "## 0.1.0".
	/^## / {
		line = $0
		sub(/^## +/, "", line)
		gsub(/[][]/, "", line)
		split(line, parts, / /)
		found = (parts[1] == want)
		if (found) { printing = 1; next }
		if (printing) { exit }
	}
	# The link reference definitions at the foot of the file belong to the
	# whole changelog, not to the last section in it.
	printing && /^\[[^]]+\]: / { exit }
	printing { print }
' "$changelog")

# Trim the blank lines the section ends with, so the release page does not open
# on whitespace.
notes=$(printf '%s\n' "$notes" | sed -e '/./,$!d' | awk 'NF {blank = 0; hold[++n] = $0; next} {blank++; hold[++n] = $0} END {for (i = 1; i <= n - blank; i++) print hold[i]}')

if [ -z "$notes" ]; then
	echo "release-notes: $changelog has no section for $version — add one before tagging" >&2
	exit 1
fi

printf '%s\n' "$notes"
