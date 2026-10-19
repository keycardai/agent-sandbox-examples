#!/usr/bin/env bash
# The license gate: every third-party module in this module's build list must
# have a license on the allow-list below, identified by SPDX id. go-licenses
# walks the packages' resolved imports, so a transitive dependency is checked
# like a direct one.
#
# It walks one GOOS at a time, and a file its build constraints exclude is
# never loaded. Both platforms run here, so a module that only a
# //go:build linux file imports is checked on a macOS workstation too.
#
# Usage: check-licenses.sh             check the build list
#        check-licenses.sh --allowed   print the allow-list, one id per line
#
# The allow-list is the set of licenses the shipped binaries may redistribute.
# MPL-2.0 is on it because its obligations attach to the MPL-licensed files
# themselves, which a Go module dependency leaves unmodified and unvendored.
# GPL, LGPL and AGPL are off it. A build-time tool is not on this list's
# terms: it is a tool directive in go.mod, imported by no package, so it is
# outside the packages this gate walks and in no binary.
set -euo pipefail

allowed_licenses=(
	Apache-2.0
	MIT
	BSD-2-Clause
	BSD-3-Clause
	ISC
	MPL-2.0
)

if [[ ${1-} == --allowed ]]; then
	printf '%s\n' "${allowed_licenses[@]}"
	exit 0
fi

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# The Makefile passes GO, matching its own `GO ?= go`. mise provides that go,
# at the version mise.toml pins.
go=${GO:-go}
gobin=$(command -v "$go") || {
	echo "$(basename "$0"): go not found. Run 'mise install'." >&2
	exit 1
}
# go-licenses shells out to the go command itself, so the directory holding it
# has to be on PATH for that child process, not just resolved here.
PATH=$(cd "$(dirname "$gobin")" && pwd):$PATH
export PATH

cd "$here/.."

# The tool is built once, for the host. Setting GOOS for the build would
# cross-build the tool itself instead of changing what it walks.
tool_dir=$(mktemp -d)
trap 'rm -rf "$tool_dir"' EXIT
"$go" build -o "$tool_dir/go-licenses" github.com/google/go-licenses/v2

# Packages of this repository are not third-party and are skipped by path
# prefix; their dependencies are still checked.
allowed=$(IFS=,; echo "${allowed_licenses[*]}")
for goos in linux darwin; do
	echo "check-licenses: GOOS=$goos"
	GOOS=$goos "$tool_dir/go-licenses" check ./... \
		--allowed_licenses="$allowed" \
		--ignore github.com/keycardai/agent-sandbox-examples
done
echo "check-licenses: OK: every module in the build list is licensed under one of: $allowed"
