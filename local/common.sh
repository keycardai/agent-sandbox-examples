#!/usr/bin/env bash
# Settings and checks that the local cluster scripts share. Sourced, never run.
#
# The scripts run the k3d, kubectl and jq that mise.toml pins, whatever else
# is on PATH. They stop unless mise resolves each one from mise.toml to an
# installed version.

# shellcheck source=scripts/pinned
. "$(dirname "${BASH_SOURCE[0]}")/../scripts/pinned"

# The settings local.env can hold. local.env.example documents each one.
env_keys='KEYCARD_LOCAL_CLUSTER|KEYCARD_LOCAL_ISSUER|KEYCARD_LOCAL_ZONE_URL|KEYCARD_LOCAL_USER_EMAIL'
env_keys+='|KEYCARD_LOCAL_ZONE_SETUP_VALUES|KEYCARD_LOCAL_GITHUB_LOGIN|KEYCARD_LOCAL_CA_FILE'
env_keys+='|KEYCARD_LOCAL_TUNNEL|KEYCARD_LOCAL_ZONE_ON_HOST'

# `go run ./cmd/sandbox tunnel` writes the quick tunnel's URL here while it
# runs, and removes the file when it exits.
quick_issuer_file=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/out/issuer

# load_env reads the settings file, local/local.env unless the first
# argument names another. It reads the file as data and executes nothing.
# Each line is blank, a # comment, or KEY=value for a key in env_keys. The
# value is the rest of the line, taken literally.
# Any other line stops the run. A script's flag wins over an exported
# variable of the same name, and an exported variable that is not empty wins
# over the file. A missing default file is skipped. A missing named file
# stops the run.
load_env() {
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
		if [[ $line =~ ^($env_keys)=(.*)$ ]]; then
			[[ -n ${!BASH_REMATCH[1]:-} ]] || printf -v "${BASH_REMATCH[1]}" '%s' "${BASH_REMATCH[2]}"
		else
			echo "$(basename "$0"): $file line $n is not KEY=value for a key local.env.example lists" >&2
			exit 1
		fi
	done <"$file"
}

# local_issuer prints KEYCARD_LOCAL_ISSUER when it is set. Otherwise it
# prints the quick tunnel URL in quick_issuer_file, or nothing when that file
# does not exist.
local_issuer() {
	if [[ -n ${KEYCARD_LOCAL_ISSUER:-} ]]; then
		printf '%s\n' "$KEYCARD_LOCAL_ISSUER"
	elif [[ -f $quick_issuer_file ]]; then
		printf '%s\n' "$(<"$quick_issuer_file")"
	fi
}

# require_cluster stops a run that names no cluster, or names a cluster k3d
# would refuse or read as an option. The name rule is k3d's RFC 1123 label
# with its 32-character limit, narrowed to lowercase.
require_cluster() {
	local name=$1
	if [[ -z $name ]]; then
		echo "$(basename "$0"): no cluster name. Set KEYCARD_LOCAL_CLUSTER or pass --cluster." >&2
		exit 1
	fi
	# The classes spell out every character so that no locale widens them.
	local ld=abcdefghijklmnopqrstuvwxyz0123456789
	if ((${#name} > 32)) || [[ ! $name =~ ^[$ld]([$ld-]*[$ld])?$ ]]; then
		echo "$(basename "$0"): the cluster name must be lowercase letters, digits and dashes (got '$name')" >&2
		exit 1
	fi
}

require_tools() {
	local tool
	for tool in "$@"; do
		command -v "$tool" >/dev/null 2>&1 || {
			echo "$(basename "$0"): $tool is not installed" >&2
			exit 1
		}
	done
}

# use_pinned_tools puts the directory of each named tool that the
# repository's mise.toml pins first on PATH. It resolves every tool before it
# changes PATH, so a tool directory cannot supply the mise that resolves the
# next tool.
use_pinned_tools() {
	local tool path dirs=""
	for tool in "$@"; do
		path=$(pinned_path "$tool") || exit 1
		dirs=$(dirname "$path"):$dirs
	done
	export PATH=$dirs$PATH
}

# cluster_exists returns 0 when k3d lists a cluster named exactly $1 and 1
# when it does not. A failed listing, or output that is not a list of named
# clusters, stops the run, so an unreachable Docker never reads as a missing
# cluster.
cluster_exists() {
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

# cluster_gateway prints the gateway of the k3d cluster's Docker network. A
# pod in that cluster reaches the Docker host at this address. Read it on
# every run. A recreated cluster can get a different Docker subnet. It prints
# the first IPv4 gateway, because the zone's host alias takes one address.
cluster_gateway() {
	local gateway
	gateway=$(docker network inspect "k3d-$1" --format '{{range .IPAM.Config}}{{println .Gateway}}{{end}}' |
		grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | head -1) || gateway=""
	if [[ -z $gateway ]]; then
		echo "$(basename "$0"): the Docker network k3d-$1 has no IPv4 gateway. Does the cluster exist?" >&2
		exit 1
	fi
	printf '%s\n' "$gateway"
}

# ca_file prints KEYCARD_LOCAL_CA_FILE, the PEM file of extra trusted roots.
# It prints nothing when the setting is empty. A setting that is not an
# absolute path to a readable file with a PEM certificate stops the run. The
# path must be absolute because the scripts pass it to commands that run in
# another directory.
ca_file() {
	local file=${KEYCARD_LOCAL_CA_FILE:-}
	[[ -n $file ]] || return 0
	if [[ $file != /* ]]; then
		echo "$(basename "$0"): KEYCARD_LOCAL_CA_FILE must be an absolute path (got '$file')" >&2
		exit 1
	fi
	if [[ ! -f $file || ! -r $file ]]; then
		echo "$(basename "$0"): KEYCARD_LOCAL_CA_FILE $file is not a readable file" >&2
		exit 1
	fi
	if [[ ! $(<"$file") =~ (^|$'\n')-----BEGIN\ CERTIFICATE----- ]]; then
		echo "$(basename "$0"): KEYCARD_LOCAL_CA_FILE $file does not hold a PEM certificate" >&2
		exit 1
	fi
	printf '%s\n' "$file"
}
