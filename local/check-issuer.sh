#!/usr/bin/env bash
# Mints a ServiceAccount token in the local k3d cluster and verifies it with
# cmd/oidc-check against the issuer's https URL, as the Keycard zone does.
#
# It passes only while the issuer's discovery document and JWKS answer at
# that URL. oidc-check trusts the system roots, and the roots in
# KEYCARD_LOCAL_CA_FILE when it is set.
#
# Usage: check-issuer.sh [--cluster NAME] [--issuer URL] [--env FILE]
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(dirname "$here")

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

# shellcheck source=local/common.sh
. "$here/common.sh"
load_env "$env_file"
cluster=${cluster:-${KEYCARD_LOCAL_CLUSTER:-}}
issuer=${issuer:-${KEYCARD_LOCAL_ISSUER:-}}

require_cluster "$cluster"
[[ -n $issuer ]] || die "no issuer. Set KEYCARD_LOCAL_ISSUER or pass --issuer."
ca=$(ca_file)
use_pinned_tools kubectl go
context=k3d-$cluster
# Turn off kuberc. A kuberc default for --server, --cluster, --user or --token
# would override the cluster and credentials of the --context context.
export KUBERC=off

token=$(kubectl --context "$context" -n default create token default --audience oidc-check --duration 10m)
ca_args=()
[[ -z $ca ]] || ca_args=(--ca-file "$ca")
go -C "$repo" run ./cmd/oidc-check --issuer "$issuer" --token-file - --audience oidc-check \
	--subject system:serviceaccount:default:default ${ca_args[@]+"${ca_args[@]}"} <<<"$token"
