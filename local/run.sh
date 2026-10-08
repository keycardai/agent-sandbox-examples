#!/usr/bin/env bash
# Runs the sandbox flow end to end on a local k3d cluster: the cluster, the
# three images, zone-setup against the Keycard zone, the install chart,
# new-sandbox for one user, and the end-to-end checks. It writes to the local
# cluster and to the zone.
#
# It does not publish the issuer. The zone fetches
# <issuer>/.well-known/openid-configuration for every exchange. Keep
# `go run ./cmd/sandbox mirror` serving, and `go run ./cmd/sandbox tunnel` or
# your own proxy forwarding the issuer's https URL to it, before the checks
# run. `go run ./cmd/sandbox check-issuer` verifies both.
#
# The sandbox pod dials the zone at the address its host name resolves to.
# With KEYCARD_LOCAL_ZONE_ON_HOST=true, it dials the gateway of the cluster's
# Docker network instead, for a zone that answers on the Docker host. The
# proxy trusts the system roots, and the roots in KEYCARD_LOCAL_CA_FILE when
# it is set.
#
# --skip-zone-setup reuses local/out/zone-setup.yaml from an earlier run.
# --replace-active-policy-set passes that flag to zone-setup, which then
# replaces any other active policy set in the zone.
#
# Usage: run.sh [--env FILE] [--skip-zone-setup] [--replace-active-policy-set]
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/.." && pwd)
# The ConfigMap that holds KEYCARD_LOCAL_CA_FILE for the proxy.
readonly zone_trust=egress-zone-trust
readonly images=(
	ghcr.io/keycardai/egress-netsetup:local
	ghcr.io/keycardai/egress-proxy:local
	ghcr.io/keycardai/sandbox-base:local
)

env_file=""
skip_zone_setup=false
zone_setup_args=()

die() {
	echo "run.sh: $*" >&2
	exit 1
}

step() {
	printf '\n==> %s\n' "$*"
}

while [[ $# -gt 0 ]]; do
	case $1 in
	--env) [[ $# -ge 2 ]] || die "--env needs a value"; env_file=$2; shift 2 ;;
	--skip-zone-setup) skip_zone_setup=true; shift ;;
	--replace-active-policy-set) zone_setup_args+=("$1"); shift ;;
	-h | --help) awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; exit 0 ;;
	*) die "unknown argument $1. --help lists them." ;;
	esac
done

# shellcheck source=local/common.sh
. "$here/common.sh"
load_env "$env_file"
cluster=${KEYCARD_LOCAL_CLUSTER:-}
issuer=$(local_issuer)
zone_url=${KEYCARD_LOCAL_ZONE_URL:-}
user_email=${KEYCARD_LOCAL_USER_EMAIL:-}
zone_on_host=${KEYCARD_LOCAL_ZONE_ON_HOST:-}
zone_values=$repo/${KEYCARD_LOCAL_ZONE_SETUP_VALUES:-zone-setup.yaml}

require_cluster "$cluster"
[[ -n $issuer ]] || die "no issuer. Set KEYCARD_LOCAL_ISSUER, or start go run ./cmd/sandbox tunnel."
[[ $zone_url =~ ^https://[a-z0-9.-]+$ ]] ||
	die "KEYCARD_LOCAL_ZONE_URL must be https://<zone host> (got '$zone_url'). run.sh does not create a zone."
[[ -n $user_email ]] || die "no user. Set KEYCARD_LOCAL_USER_EMAIL to an email zone-setup enrolls."
[[ -z $zone_on_host || $zone_on_host == true ]] ||
	die "KEYCARD_LOCAL_ZONE_ON_HOST must be true or empty (got '$zone_on_host')"
ca=$(ca_file)
require_tools docker
use_pinned_tools go k3d kubectl yq
export KUBERC=off
context=k3d-$cluster
kube() { kubectl --context "$context" "$@"; }
out=$here/out
mkdir -p "$out"

env_args=()
[[ -z $env_file ]] || env_args=(--env "$env_file")

step "the k3d cluster $cluster"
go -C "$repo" run ./cmd/sandbox ${env_args[@]+"${env_args[@]}"} bootstrap --cluster "$cluster" --issuer "$issuer"

step "the three images"
missing=0
for image in "${images[@]}"; do
	if ! docker image inspect "$image" >/dev/null 2>&1; then
		echo "    missing $image" >&2
		missing=$((missing + 1))
	fi
done
((missing == 0)) || die "$missing image(s) missing. Build them in the egress-proxy repository and tag them with the names above."
k3d image import --cluster "$cluster" "${images[@]}"

if [[ $skip_zone_setup == false ]]; then
	step "zone-setup against $zone_url"
	[[ -f $zone_values ]] || die "$zone_values does not exist. Start from zone-setup.example.yaml."
	go -C "$repo" run ./cmd/zone-setup --values "$zone_values" --issuer-url "$issuer" \
		${zone_setup_args[@]+"${zone_setup_args[@]}"} >"$out/zone-setup.yaml"
fi
[[ -s $out/zone-setup.yaml ]] || die "$out/zone-setup.yaml is empty. Run without --skip-zone-setup."

# Every id from here on comes from the zone-setup output.
output() {
	EMAIL=$user_email yq -e "$1" "$out/zone-setup.yaml" ||
		die "the zone-setup output has no $1 for $user_email"
}
user='.sandbox_user_access[] | select(.user_email == strenv(EMAIL))'
namespace=$(output '.k8s_sandbox.namespace')
provider_id=$(output '.k8s_sandbox.provider_id')
application_id=$(output '.k8s_sandbox.application_id')
application_identifier=$(output '.k8s_sandbox.application_identifier')
user_id=$(output "$user | .user_id")
service_account=$(output "$user | .service_account")
client_id=$(output "$user | .client_id")

step "the values from the running cluster"
if [[ $zone_on_host == true ]]; then
	gateway=$(cluster_gateway "$cluster")
	zone_host=${zone_url#https://}
	echo "    $zone_host maps to $gateway, the k3d-$cluster gateway"
	alias="hostAliases:
  - ip: $gateway
    hostnames: [$zone_host]"
fi
{
	echo "zoneURL: $zone_url"
	if [[ -n $ca ]]; then
		echo "zoneTrust:"
		echo "  configMap: $zone_trust"
		echo "  certificate: |"
		sed 's/^/    /' "$ca"
	fi
	if [[ $zone_on_host == true ]]; then
		echo "$alias"
		echo "network:"
		echo "  allowCIDRs: [\"$gateway/32\"]"
	fi
} >"$out/install.values.yaml"
# The gateway is in a private range. The proxy refuses to dial a zone address
# on the platform deny list.
{
	echo "zoneURL: $zone_url"
	if [[ -n $ca ]]; then
		echo "zoneTrust:"
		echo "  configMap: $zone_trust"
	fi
	if [[ $zone_on_host == true ]]; then
		echo "$alias"
		echo "policy:"
		echo "  denyListExceptions: [\"$gateway/32\"]"
	fi
} >"$out/sandbox.values.yaml"

step "install.sh into $namespace"
"$repo/install/install.sh" --context "$context" --namespace "$namespace" \
	--values "$here/values/install.yaml" --values "$out/install.values.yaml"

step "new-sandbox for $user_email ($user_id)"
go -C "$repo" run ./cmd/new-sandbox \
	--values "$repo/sandbox/values.example.yaml" \
	--values "$here/values/sandbox.yaml" \
	--values "$out/sandbox.values.yaml" \
	--chart "$repo/sandbox" \
	--kube-context "$context" \
	--namespace "$namespace" \
	--user-id "$user_id" \
	--user-email "$user_email" \
	--service-account "$service_account" \
	--client-id "$client_id" \
	--provider-id "$provider_id" \
	--application-id "$application_id" \
	--application-identifier "$application_identifier" \
	>"$out/sandbox.yaml"

step "waiting for the sandbox pod"
kube -n "$namespace" wait --for=create "pod/$user_id" --timeout=120s >/dev/null
if ! kube -n "$namespace" wait --for=condition=Ready "pod/$user_id" --timeout=300s; then
	for container in egress-netsetup egress-proxy agent; do
		echo "--- $container" >&2
		kube -n "$namespace" logs "$user_id" -c "$container" --tail=40 >&2 || true
	done
	die "the sandbox pod did not become ready"
fi

step "the end-to-end checks"
exec "$here/checks.sh" --cluster "$cluster" --namespace "$namespace" --pod "$user_id" --user-id "$user_id" \
	${env_args[@]+"${env_args[@]}"}
