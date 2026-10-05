#!/usr/bin/env bash
# Snapshots the lane cluster's issuer discovery document and JWKS into
# local/out/oidc/<cluster>, then serves them with cmd/oidc-mirror in the
# foreground until interrupted. It only reads from the cluster.
#
# k3s keeps one signing key for the cluster's life, so the snapshot stays
# valid until the cluster is recreated. Rerun this after recreating it.
#
# The Keycard zone fetches the issuer through the localdev Gateway, which
# reaches the mirror only once the route that route.sh prints is applied to
# localdev. Apply it yourself. This lane never writes to localdev.
#
# The default loopback address works on OrbStack, where a 127.0.0.1
# listener on the host answers containers on the default bridge and on the
# k3d-localdev network through host-gateway. Docker Desktop is not verified.
# If the Gateway cannot reach the mirror there, pass --listen 0.0.0.0:8480.
# That exposes only the two public documents, and no credential.
#
# Usage: mirror.sh [--cluster NAME] [--issuer URL] [--listen ADDR] [--env FILE]
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(dirname "$here")

cluster=""
issuer=""
listen=127.0.0.1:8480
env_file=""

die() {
	echo "mirror.sh: $*" >&2
	exit 1
}

while [[ $# -gt 0 ]]; do
	case $1 in
	--cluster) cluster=$2; shift 2 ;;
	--issuer) issuer=$2; shift 2 ;;
	--listen) listen=$2; shift 2 ;;
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
lane_use_pinned kubectl go
context=k3d-$cluster
out=$here/out/oidc/$cluster
# kubectl's kuberc can override the cluster that --context names, so it does not
# apply here.
export KUBERC=off

mkdir -p "$out"
kubectl --context "$context" get --raw /.well-known/openid-configuration >"$out/openid-configuration.json" ||
	die "cannot read the discovery document from $context"
kubectl --context "$context" get --raw /openid/v1/jwks >"$out/jwks.json" ||
	die "cannot read the JWKS from $context"

exec go -C "$repo" run ./cmd/oidc-mirror --issuer "$issuer" --dir "$out" --listen "$listen"
