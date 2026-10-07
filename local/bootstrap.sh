#!/usr/bin/env bash
# Creates the local k3d cluster with the ServiceAccount issuer baked into its
# API server, and checks that a minted token has it.
#
# The API server cannot change its issuer while it runs, so an existing
# cluster is verified rather than recreated. A cluster whose discovery
# document names a different issuer or jwks_uri stops the run.
#
# --dry-run validates the settings and prints the k3d command without
# touching Docker or any cluster.
#
# Usage: bootstrap.sh [--issuer URL] [--cluster NAME] [--env FILE] [--dry-run]
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# k3s stable channel release, verified with k3d 5.9.0.
readonly k3s_image=rancher/k3s:v1.36.4-k3s1

cluster=""
issuer=""
env_file=""
dry_run=false

die() {
	echo "bootstrap.sh: $*" >&2
	exit 1
}

while [[ $# -gt 0 ]]; do
	case $1 in
	--issuer) issuer=$2; shift 2 ;;
	--cluster) cluster=$2; shift 2 ;;
	--env) env_file=$2; shift 2 ;;
	--dry-run) dry_run=true; shift ;;
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
[[ $issuer == https://* ]] || die "the issuer must be https (got '$issuer')"
[[ $issuer != *\?* && $issuer != *\#* ]] || die "the issuer must have no query or fragment (got '$issuer')"
[[ $issuer != */ ]] || die "the issuer must have no trailing slash (got '$issuer')"
# The classes spell out every character so that no locale widens them.
lower=abcdefghijklmnopqrstuvwxyz
upper=ABCDEFGHIJKLMNOPQRSTUVWXYZ
digit=0123456789
label="[$lower$digit]([$lower$digit-]*[$lower$digit])?"
host_re="^https://$label(\\.$label)*(:|/|\$)"
issuer_re="^https://$label(\\.$label)*(:([123456789][$digit]{0,4}))?((/[$upper$lower$digit._~-]+)*)\$"
[[ $issuer =~ $host_re ]] ||
	die "the issuer must name a host of lowercase ASCII DNS labels (got '$issuer')"
[[ $issuer =~ $issuer_re ]] ||
	die "the issuer must have an optional port and a path of A-Z a-z 0-9 . _ ~ - segments (got '$issuer')"
port_re=":([$digit]+)"
if [[ ${issuer#https://} =~ $port_re ]]; then
	((BASH_REMATCH[1] <= 65535)) || die "the issuer port must be 1-65535 (got '$issuer')"
fi
jwks_uri=$issuer/openid/v1/jwks

# `@server:*` sends each flag to every server node, and k3s forwards
# --kube-apiserver-arg to kube-apiserver. The public issuer replaces k3s's
# default, so kube-apiserver mints tokens with it. The local cluster serves
# nothing through an ingress or a LoadBalancer Service, so k3s's Traefik and
# servicelb stay off.
create=(
	k3d cluster create "$cluster"
	--image "$k3s_image"
	--kubeconfig-update-default=true
	--kubeconfig-switch-context=false
	--k3s-arg "--kube-apiserver-arg=service-account-issuer=$issuer@server:*"
	--k3s-arg "--kube-apiserver-arg=service-account-jwks-uri=$jwks_uri@server:*"
	--k3s-arg "--disable=traefik@server:*"
	--k3s-arg "--disable=servicelb@server:*"
)
if [[ $dry_run == true ]]; then
	echo "bootstrap.sh: would run:"
	printf '%s\n' "${create[@]}"
	exit 0
fi

require_tools docker
use_pinned_tools jq k3d kubectl
context=k3d-$cluster
# Turn off kuberc. A kuberc default for --server, --cluster, --user or --token
# would override the cluster and credentials of the --context context.
export KUBERC=off
kube() { kubectl --context "$context" "$@"; }

if cluster_exists "$cluster"; then
	echo "bootstrap.sh: reusing the k3d cluster $cluster"
	discovery=$(kube get --raw /.well-known/openid-configuration) ||
		die "cannot read the discovery document from $context, so its issuer cannot be confirmed"
	have_issuer=$(jq -r '.issuer // empty' <<<"$discovery")
	have_jwks=$(jq -r '.jwks_uri // empty' <<<"$discovery")
	if [[ $have_issuer != "$issuer" || $have_jwks != "$jwks_uri" ]]; then
		die "cluster $cluster was baked with different settings:

    issuer   : $have_issuer (wanted $issuer)
    jwks_uri : $have_jwks (wanted $jwks_uri)

    The API server cannot change them. Delete the cluster with
    ./local/down.sh --cluster $cluster, or bootstrap with its issuer."
	fi
else
	echo "bootstrap.sh: creating $cluster ($k3s_image) with issuer $issuer"
	"${create[@]}"
fi

kube -n default wait --for=create serviceaccount/default --timeout=120s >/dev/null
token=$(kube -n default create token default --duration 10m)
token_iss=$(jq -rR 'split(".")[1] | gsub("-"; "+") | gsub("_"; "/") | @base64d | fromjson | .iss // empty' <<<"$token")
[[ $token_iss == "$issuer" ]] || die "the API server mints iss '$token_iss', not $issuer"
echo "bootstrap.sh: $cluster mints iss $token_iss. Use kubectl --context $context."
