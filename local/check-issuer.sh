#!/usr/bin/env bash
# Mints a ServiceAccount token in the lane cluster and verifies it with
# cmd/oidc-check against the published issuer.
#
# It fetches the issuer through the localdev Gateway, as the Keycard zone
# does, so it passes only while mirror.sh is serving and the route that
# route.sh prints is applied to localdev. It trusts the localdev root CA
# from ~/.localdev/devcert.json.
#
# Usage: check-issuer.sh [--cluster NAME] [--issuer URL] [--env FILE]
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(dirname "$here")
devcert=$HOME/.localdev/devcert.json

cluster=""
issuer=""
env_file=""

die() {
	echo "check-issuer.sh: $*" >&2
	exit 1
}

while [[ $# -gt 0 ]]; do
	case $1 in
	--cluster) cluster=$2; shift 2 ;;
	--issuer) issuer=$2; shift 2 ;;
	--env) env_file=$2; shift 2 ;;
	-h | --help) awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; exit 0 ;;
	*) die "unknown argument $1. --help lists them." ;;
	esac
done

# shellcheck source=local/lane.sh
. "$here/lane.sh"
lane_load_env "$env_file"
cluster=${cluster:-${KEYCARD_LOCAL_CLUSTER:-}}
issuer=${issuer:-${KEYCARD_LOCAL_ISSUER:-}}

lane_require_cluster "$cluster"
[[ -n $issuer ]] || die "no issuer. Set KEYCARD_LOCAL_ISSUER or pass --issuer."
[[ -f $devcert ]] || die "$devcert does not exist, so the localdev root CA is unknown. Is localdev set up?"
lane_use_pinned kubectl jq go
context=k3d-$cluster
# Turn off kuberc. A kuberc default for --server, --cluster, --user or --token
# would override the cluster and credentials of the --context context.
export KUBERC=off

ca=$(mktemp)
trap 'rm -f "$ca"' EXIT
jq -er .root_certificate_pem "$devcert" >"$ca" || die "$devcert holds no root_certificate_pem"

token=$(kubectl --context "$context" -n default create token default --audience oidc-check --duration 10m)
go -C "$repo" run ./cmd/oidc-check --issuer "$issuer" --token-file - --audience oidc-check \
	--subject system:serviceaccount:default:default --ca-file "$ca" <<<"$token"
