#!/usr/bin/env bash
# Deletes the local lane's k3d cluster. It refuses the name localdev.
#
# Usage: down.sh [--cluster NAME] [--env FILE] [--yes]
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

cluster=""
env_file=""
confirmed=false

die() {
	echo "down.sh: $*" >&2
	exit 1
}

while [[ $# -gt 0 ]]; do
	case $1 in
	--cluster) cluster=$2; shift 2 ;;
	--env) env_file=$2; shift 2 ;;
	--yes | -y) confirmed=true; shift ;;
	-h | --help) awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; exit 0 ;;
	*) die "unknown argument $1. --help lists them." ;;
	esac
done

# shellcheck source=local/lane.sh
. "$here/lane.sh"
lane_load_env "$env_file"
cluster=${cluster:-${KEYCARD_LOCAL_CLUSTER:-}}
lane_require_cluster "$cluster"
lane_require_tools docker k3d jq

if ! lane_cluster_exists "$cluster"; then
	echo "down.sh: there is no k3d cluster named $cluster"
	exit 0
fi

if [[ $confirmed == false ]]; then
	printf 'down.sh: delete the k3d cluster %s? [y/N] ' "$cluster"
	read -r answer
	[[ $answer == [yY]* ]] || die "nothing deleted"
fi

k3d cluster delete "$cluster"
echo "down.sh: deleted $cluster"
