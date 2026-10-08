#!/usr/bin/env bash
# Snapshots the local k3d cluster's issuer discovery document and JWKS into
# local/out/oidc/<cluster>, then serves them over plain HTTP on a local
# address with cmd/oidc-mirror. It runs in the foreground until interrupted,
# and it only reads from the cluster.
#
# k3s keeps one signing key for the cluster's life, so the snapshot stays
# valid until the cluster is recreated. Rerun this after recreating it.
#
# The Keycard zone fetches both documents from the issuer's https URL.
# tunnel.sh forwards that URL to the mirror through a Cloudflare tunnel. To
# publish it another way, use a TLS-terminating proxy for the issuer host
# that forwards to --listen.
#
# The default address is loopback. On OrbStack, a 127.0.0.1 listener on the
# host answers containers on Docker's default bridge through host-gateway.
# Docker Desktop is not verified. If your proxy gets no answer from the
# mirror, pass --listen 0.0.0.0:8480. That exposes only the two public
# documents, and no credential.
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

# shellcheck source=local/common.sh
. "$here/common.sh"
load_env "$env_file"
cluster=${cluster:-${KEYCARD_LOCAL_CLUSTER:-}}
issuer=${issuer:-$(local_issuer)}

require_cluster "$cluster"
[[ -n $issuer ]] || die "no issuer. Set KEYCARD_LOCAL_ISSUER, pass --issuer, or start tunnel.sh."
use_pinned_tools kubectl go
context=k3d-$cluster
out=$here/out/oidc/$cluster
# Turn off kuberc. A kuberc default for --server, --cluster, --user or --token
# would override the cluster and credentials of the --context context.
export KUBERC=off

mkdir -p "$out"
kubectl --context "$context" get --raw /.well-known/openid-configuration >"$out/openid-configuration.json" ||
	die "cannot read the discovery document from $context"
kubectl --context "$context" get --raw /openid/v1/jwks >"$out/jwks.json" ||
	die "cannot read the JWKS from $context"

exec go -C "$repo" run ./cmd/oidc-mirror --issuer "$issuer" --dir "$out" --listen "$listen"
