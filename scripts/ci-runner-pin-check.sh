#!/bin/sh
# Copyright 2026 the AMD Halo Operator authors
# SPDX-License-Identifier: Apache-2.0
#
# Asserts that every job container referenced by a workflow in this repository
# is pinned to an immutable image reference.
#
# Why this exists: a mutable tag makes a green build unnameable. ":latest"
# passing CI on Monday says nothing about what runs on Tuesday, and when it
# breaks there is no way to state which image was tested. The fix is to pin,
# and the check is what keeps the pin.
#
# THE RULE IS A POSITIVE LIST. A reference must MATCH a recognised immutable
# form to pass. It is deliberately not a list of known-mutable tags to reject:
# a blacklist passes silently the first time someone invents a new moving tag
# (":edge", ":rolling", ":main"), which is exactly when the check is needed
# most. Unrecognised means rejected here, even if the new form is in fact
# immutable — adding a form is a deliberate edit to this file.
#
# Recognised immutable forms:
#   name:<40 hex>                      a git commit SHA tag
#   name@sha256:<64 hex>               a content digest
#   name:<40 hex>@sha256:<64 hex>      both
#
# A commit SHA tag is preferred over a bare digest: both are immutable, but
# only the commit SHA can be named — `git show <sha>` explains what is in the
# image, which is the whole point of being able to state what was tested.
#
# Portability: POSIX sh and awk only. Not bash — `#!/usr/bin/env bash` resolves
# bash via PATH, and on a macOS arm64 machine with an Intel Homebrew in
# /usr/local that finds a binary which dies with "Bad CPU type in executable".
# A check that cannot run on a developer's machine is a check they stop
# trusting. Also no python3: the CI runner image is ubuntu:24.04, which does
# not ship one.

set -eu

cd "$(dirname "$0")/.."

# The floor. Zero references found must be a FAILURE, not a pass: if a workflow
# is reshaped, renamed, or this extractor silently stops matching, an empty
# result set would otherwise satisfy "every reference is pinned" vacuously and
# the check would report success having verified nothing.
MIN_REFS="${MIN_REFS:-1}"

workflow_dir=".github/workflows"
if [ ! -d "$workflow_dir" ]; then
	echo "ci-runner-pin-check: $workflow_dir does not exist" >&2
	exit 1
fi

# Collect the workflow files without a glob that could expand to a literal.
files=""
for f in "$workflow_dir"/*.yml "$workflow_dir"/*.yaml; do
	[ -f "$f" ] || continue
	files="$files $f"
done
if [ -z "$files" ]; then
	echo "ci-runner-pin-check: no workflow files under $workflow_dir" >&2
	exit 1
fi

# Extract every container image reference, handling both YAML spellings:
#
#   container: image:tag            (scalar)
#   container:                      (mapping)
#     image: image:tag
#
# A `container:` key whose image cannot be resolved is emitted as UNRESOLVED
# rather than skipped, so the mapping form changing shape fails loudly instead
# of quietly reducing the set this check covers to nothing.
refs="$(awk '
	function indent_of(s) { match(s, /^[ \t]*/); return RLENGTH }

	# Leaving an unresolved container: block — same or shallower indent.
	in_container && indent_of($0) <= container_indent && $0 !~ /^[ \t]*$/ {
		print "UNRESOLVED:" FILENAME ":" container_line
		in_container = 0
	}

	# The image: key inside a container: mapping.
	in_container && $0 ~ /^[ \t]*image:[ \t]*[^ \t]/ {
		line = $0
		sub(/^[ \t]*image:[ \t]*/, "", line)
		gsub(/["\047]/, "", line)
		sub(/[ \t]+$/, "", line)
		print FILENAME ":" FNR ":" line
		in_container = 0
		next
	}

	# container: with a value on the same line.
	$0 ~ /^[ \t]*container:[ \t]*[^ \t]/ {
		line = $0
		sub(/^[ \t]*container:[ \t]*/, "", line)
		gsub(/["\047]/, "", line)
		sub(/[ \t]+$/, "", line)
		print FILENAME ":" FNR ":" line
		next
	}

	# A bare container: key opens a mapping.
	$0 ~ /^[ \t]*container:[ \t]*$/ {
		in_container = 1
		container_indent = indent_of($0)
		container_line = FNR
		next
	}

	END {
		if (in_container) print "UNRESOLVED:" FILENAME ":" container_line
	}
' $files)"

count=0
bad=0
while IFS= read -r entry; do
	[ -n "$entry" ] || continue

	case "$entry" in
	UNRESOLVED:*)
		echo "FAIL ${entry#UNRESOLVED:}: a container: block whose image could not be resolved" >&2
		echo "     the extractor in $0 does not understand this shape; fix one or the other" >&2
		bad=$((bad + 1))
		continue
		;;
	esac

	# entry is file:line:ref — split off the ref, which may itself contain ':'.
	where="$(printf '%s\n' "$entry" | cut -d: -f1,2)"
	ref="$(printf '%s\n' "$entry" | cut -d: -f3-)"

	# A templated reference cannot be judged here; it is resolved at run time.
	case "$ref" in
	*'${{'*)
		echo "FAIL $where: '$ref' is templated, so the pin cannot be verified statically" >&2
		bad=$((bad + 1))
		count=$((count + 1))
		continue
		;;
	esac

	count=$((count + 1))

	if printf '%s\n' "$ref" | grep -Eq \
		-e '^[^[:space:]]+:[0-9a-f]{40}$' \
		-e '^[^[:space:]]+@sha256:[0-9a-f]{64}$' \
		-e '^[^[:space:]]+:[0-9a-f]{40}@sha256:[0-9a-f]{64}$'; then
		echo "ok   $where: $ref"
	else
		echo "FAIL $where: '$ref' is not a recognised immutable reference" >&2
		echo "     use name:<40-hex commit sha>, name@sha256:<64 hex>, or both" >&2
		bad=$((bad + 1))
	fi
done <<EOF
$refs
EOF

if [ "$count" -lt "$MIN_REFS" ]; then
	echo "ci-runner-pin-check: found $count container reference(s), expected at least $MIN_REFS" >&2
	echo "     an empty result set is a failure, not a vacuous pass — either a workflow" >&2
	echo "     stopped declaring a container, or this extractor stopped matching." >&2
	exit 1
fi

if [ "$bad" -gt 0 ]; then
	echo "ci-runner-pin-check: $bad of $count container reference(s) are not immutably pinned" >&2
	exit 1
fi

echo "ci-runner-pin-check: $count container reference(s), all immutably pinned"
