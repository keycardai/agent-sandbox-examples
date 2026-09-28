#!/usr/bin/env bash
# Settings and guards the local lane's scripts share. Sourced, never run.
#
# NEVER let any of these scripts write to the `localdev` cluster. It is the
# user's Keycard stack, and this lane is a client of it.

# lane_load_env reads the settings file, local/local.env unless the first
# argument names another. It reads the file as data and executes nothing.
# Each line is blank, a # comment, or KEY=value for KEYCARD_LOCAL_CLUSTER or
# KEYCARD_LOCAL_ISSUER. The value is the rest of the line, taken literally.
# Any other line stops the run. A script's flag wins over the file, and the
# file wins over an exported variable of the same name. A missing default
# file is skipped. A missing named file stops the run.
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
			printf -v "${BASH_REMATCH[1]}" '%s' "${BASH_REMATCH[2]}"
		else
			echo "$(basename "$0"): $file line $n is not KEYCARD_LOCAL_CLUSTER=value or KEYCARD_LOCAL_ISSUER=value" >&2
			exit 1
		fi
	done <"$file"
}

# lane_require_cluster stops a run that names no cluster or names the user's
# Keycard stack.
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
