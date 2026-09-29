#!/usr/bin/env bash
# Installs the agent-sandbox controller, then the keycard-sandbox chart.
#
# The controller comes from the release manifest committed in agent-sandbox/,
# which agent-sandbox/refresh.sh downloads and checks. Sandboxes are the
# controller's kinds, so the controller goes first. The chart adopts a
# namespace that already exists. The run stops unless the namespace then
# enforces the privileged Pod Security level, which the egress proxy's init
# container needs.
#
# NEVER install into the Keycard localdev stack. The contexts localdev and
# k3d-localdev are refused.
#
# Usage: install.sh --context NAME --values FILE [--namespace NAME]
#
#   --context NAME    the kube context to install into
#   --values FILE     the chart's values file. Start from values.example.yaml.
#   --namespace NAME  the sandbox namespace, agents unless given. It replaces
#                     the namespace in the values file.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
release=keycard-sandbox
controller_manifest=$here/agent-sandbox/sandbox-with-extensions.yaml

context=""
values=""
namespace=agents

die() {
	echo "install.sh: $*" >&2
	exit 1
}

# need stops the run when the flag $2 has no value. $1 is the count of
# arguments left, the flag included.
need() {
	[[ $1 -ge 2 ]] || die "$2 needs a value"
}

while [[ $# -gt 0 ]]; do
	case $1 in
	--context) need $# "$1"; context=$2; shift 2 ;;
	--values) need $# "$1"; values=$2; shift 2 ;;
	--namespace) need $# "$1"; namespace=$2; shift 2 ;;
	-h | --help) awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; exit 0 ;;
	*) die "unknown argument $1. --help lists them." ;;
	esac
done

[[ -n $context ]] || die "--context NAME is required. It names the cluster to install into."
case $context in
localdev | k3d-localdev) die "'$context' is the Keycard localdev stack. install.sh never writes to it." ;;
-*) die "the context must not start with a dash (got '$context')" ;;
esac
[[ -n $values ]] || die "--values FILE is required. Start from $here/values.example.yaml."
[[ -f $values && -r $values ]] || die "$values is not a readable file"
# The classes spell out every character so that no locale widens them.
ld=abcdefghijklmnopqrstuvwxyz0123456789
if ((${#namespace} > 63)) || [[ ! $namespace =~ ^[$ld]([$ld-]*[$ld])?$ ]]; then
	die "the namespace must be a DNS label of lowercase letters, digits and dashes (got '$namespace')"
fi

# shellcheck source=scripts/pinned
. "$here/../scripts/pinned"
kubectl=$(pinned_path kubectl) || exit 1
helm=$(pinned_path helm) || exit 1
kube() { "$kubectl" --context "$context" "$@"; }
# These helm variables override the cluster or identity that --kube-context names.
unset HELM_KUBEAPISERVER HELM_KUBEASGROUPS HELM_KUBEASUSER HELM_KUBECAFILE HELM_KUBECONTEXT \
	HELM_KUBEINSECURE_SKIP_TLS_VERIFY HELM_KUBETLS_SERVER_NAME HELM_KUBETOKEN HELM_NAMESPACE

echo "install.sh: applying the agent-sandbox controller to $context"
kube apply --server-side -f "$controller_manifest"
kube -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s

echo "install.sh: installing $release into namespace $namespace"
"$helm" upgrade --install "$release" "$here" \
	--kube-context "$context" \
	--namespace "$namespace" --create-namespace --take-ownership \
	--values "$values" --set-string "namespace=$namespace" \
	--wait

enforce=$(kube get namespace "$namespace" -o 'jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}') ||
	die "cannot read the labels of namespace $namespace"
if [[ $enforce != privileged ]]; then
	die "namespace $namespace enforces the Pod Security level '${enforce:-unset}', not privileged, so no sandbox will be admitted.
    Find what rewrites pod-security.kubernetes.io/enforce on the namespace, then run install.sh again."
fi
echo "install.sh: installed $release into namespace $namespace on $context"
