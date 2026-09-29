#!/usr/bin/env bash
# Settings and guards the local lane's scripts share. Sourced, never run.
#
# NEVER let any of these scripts write to the `localdev` cluster. It is the
# user's Keycard stack, and this lane is a client of it.
#
# The scripts run the k3d, kubectl and jq that mise.toml pins, whatever else
# is on PATH. They stop unless mise resolves each one from mise.toml to an
# installed version.

# lane_load_env reads the settings file, local/local.env unless the first
# argument names another. It reads the file as data and executes nothing.
# Each line is blank, a # comment, or KEY=value for KEYCARD_LOCAL_CLUSTER or
# KEYCARD_LOCAL_ISSUER. The value is the rest of the line, taken literally.
# Any other line stops the run. A script's flag wins over an exported
# variable of the same name, and an exported variable that is not empty wins
# over the file. A missing default file is skipped. A missing named file
# stops the run.
lane_load_env() {
	local file=${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/local.env}
	local line n=0
	if [[ ! -f $file ]]; then
		[[ -z ${1:-} ]] && return 0
		echo "$(basename "$0"): $file does not exist" >&2
		exit 1
	fi
	while IFS= read -r line || [[ -n $line ]]; do
		n=$((n + 1))
		[[ $line =~ ^[[:space:]]*(#|$) ]] && continue
		if [[ $line =~ ^(KEYCARD_LOCAL_CLUSTER|KEYCARD_LOCAL_ISSUER)=(.*)$ ]]; then
			[[ -n ${!BASH_REMATCH[1]:-} ]] || printf -v "${BASH_REMATCH[1]}" '%s' "${BASH_REMATCH[2]}"
		else
			echo "$(basename "$0"): $file line $n is not KEYCARD_LOCAL_CLUSTER=value or KEYCARD_LOCAL_ISSUER=value" >&2
			exit 1
		fi
	done <"$file"
}

# lane_require_cluster stops a run that names no cluster, names the user's
# Keycard stack, or names a cluster k3d would refuse or read as an option.
# The name rule is k3d's RFC 1123 label with its 32-character limit, narrowed
# to lowercase.
lane_require_cluster() {
	local name=$1
	if [[ -z $name ]]; then
		echo "$(basename "$0"): no cluster name. Set KEYCARD_LOCAL_CLUSTER or pass --cluster." >&2
		exit 1
	fi
	if [[ $name == localdev ]]; then
		echo "$(basename "$0"): 'localdev' is the Keycard localdev stack. This lane never writes to it." >&2
		exit 1
	fi
	# The classes spell out every character so that no locale widens them.
	local ld=abcdefghijklmnopqrstuvwxyz0123456789
	if ((${#name} > 32)) || [[ ! $name =~ ^[$ld]([$ld-]*[$ld])?$ ]]; then
		echo "$(basename "$0"): the cluster name must be lowercase letters, digits and dashes (got '$name')" >&2
		exit 1
	fi
}

lane_require_tools() {
	local tool
	for tool in "$@"; do
		command -v "$tool" >/dev/null 2>&1 || {
			echo "$(basename "$0"): $tool is not installed" >&2
			exit 1
		}
	done
}

# lane_use_pinned puts the directory of each named tool that the repository's
# mise.toml pins first on PATH. The answer is the same from any working
# directory.
lane_use_pinned() {
	local root mise tool path
	root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
	# Resolve mise once, so a tool directory put first on PATH cannot supply
	# another mise for the next lookup.
	mise=$(command -v mise) || {
		echo "$(basename "$0"): mise is not installed. See README.md." >&2
		exit 1
	}
	for tool in "$@"; do
		path=$("$mise" -C "$root" which "$tool" 2>/dev/null) || {
			echo "$(basename "$0"): mise does not provide $tool. Run mise install." >&2
			exit 1
		}
		lane_require_pinned "$root" "$tool" "$path" "$("$mise" -C "$root" ls --current --json "$tool" 2>/dev/null)"
		PATH=$(dirname "$path"):$PATH
	done
	export PATH
}

# lane_require_pinned stops the run unless $4, the output of
# `mise ls --current --json $2`, is one entry whose source is $1/mise.toml,
# whose version is installed, and whose install path holds $3. It matches the
# JSON with bash because jq is one of the tools it checks. The fields follow
# JSONToolVersion in mise's src/cli/ls.rs, in its order.
lane_require_pinned() {
	local root=$1 tool=$2 path=$3 json=$4
	local s='[[:space:]]*' q='"([^"\\]*)"'
	local re="^$s\\[$s\\{$s\"version\": $q,.*\"install_path\": $q,$s\"source\": \\{$s\"type\": $q,$s\"path\": $q$s\\},.*\"installed\": (true|false),$s\"active\": (true|false)$s\\}$s\\]$s$"
	if [[ $json == *'"install_path"'*'"install_path"'* || ! $json =~ $re ]]; then
		echo "$(basename "$0"): mise ls --current --json $tool printed something other than one entry" >&2
		exit 1
	fi
	local version=${BASH_REMATCH[1]} install=${BASH_REMATCH[2]} source=${BASH_REMATCH[4]}
	if [[ $source != "$root/mise.toml" ]]; then
		echo "$(basename "$0"): mise resolves $tool from $source, not $root/mise.toml" >&2
		exit 1
	fi
	if [[ ${BASH_REMATCH[5]} != true ]]; then
		echo "$(basename "$0"): $tool $version is not installed. Run mise install." >&2
		exit 1
	fi
	if [[ $path != "$install"/* ]]; then
		echo "$(basename "$0"): mise resolves $tool to $path, outside $install" >&2
		exit 1
	fi
}

# lane_cluster_exists returns 0 when k3d lists a cluster named exactly $1 and 1
# when it does not. A failed listing, or output that is not a list of named
# clusters, stops the run, so an unreachable Docker never reads as a missing
# cluster.
lane_cluster_exists() {
	local list found
	list=$(k3d cluster list -o json) || {
		echo "$(basename "$0"): cannot list k3d clusters. Is Docker running?" >&2
		exit 1
	}
	found=$(jq --arg name "$1" 'if type == "array" and all(.[]; (.name | type) == "string") then any(.[]; .name == $name) else error("not a cluster list") end' <<<"$list") || found=""
	case $found in
	true) return 0 ;;
	false) return 1 ;;
	esac
	echo "$(basename "$0"): k3d cluster list printed something other than a cluster list" >&2
	exit 1
}
