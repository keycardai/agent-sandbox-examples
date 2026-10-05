#!/usr/bin/env bash
# Renders the three objects that publish the issuer host in the Keycard
# localdev cluster, and writes them to standard output.
#
# It NEVER applies them. localdev is the user's Keycard stack and this lane
# does not write to it. Read what this prints, then apply it yourself.
#
# What each object is for:
#   Service        a name in the gateway namespace with no selector, so its
#                  endpoints are whatever the EndpointSlice says
#   EndpointSlice  one address, the localdev cluster's Docker host gateway,
#                  which is where the mirror listens
#   HTTPRoute      the exact issuer hostname on the existing Gateway. An
#                  exact hostname beats the wildcard routes already on that
#                  listener, so it takes every path under the name.
#
# Nothing else is needed. Any *.localdev.keycard.sh name already resolves
# to 127.0.0.1 through public wildcard DNS, the Gateway's https listener
# already covers *.localdev.keycard.sh, and its certificate already carries
# that SAN. The Gateway terminates TLS with it, so the mirror serves plain
# HTTP behind it.
#
# Usage: route.sh [--issuer URL] [--listen ADDR] [--host-address IP] [--env FILE]
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# The localdev Gateway's allowedRoutes selector requires gateway-access=true
# on this namespace.
readonly gateway_namespace=gateway
readonly gateway_name=gateway
readonly name=sandbox-oidc-mirror

readonly probe_image=alpine:3

issuer=""
listen=127.0.0.1:8480
host_address=""
env_file=""

die() {
	echo "route.sh: $*" >&2
	exit 1
}

while [[ $# -gt 0 ]]; do
	case $1 in
	--issuer) issuer=$2; shift 2 ;;
	--listen) listen=$2; shift 2 ;;
	--host-address) host_address=$2; shift 2 ;;
	--env) env_file=$2; shift 2 ;;
	-h | --help) awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; exit 0 ;;
	*) die "unknown argument $1. --help lists them." ;;
	esac
done

# shellcheck source=local/lane.sh
. "$here/lane.sh"
lane_load_env "$env_file"
issuer=${issuer:-${KEYCARD_LOCAL_ISSUER:-}}

[[ -n $issuer ]] || die "no issuer. Set KEYCARD_LOCAL_ISSUER or pass --issuer."

[[ $issuer == https://* ]] || die "the issuer must be https (got '$issuer')"
host=${issuer#https://}
host=${host%%/*}
# A verifier sends an issuer's explicit port, 443 included, in Host.
[[ $host != *:* ]] ||
	die "the issuer names a port, but the route matches the bare hostname on the Gateway's 443 listener (got '$issuer')"
# The classes spell out every character so that no locale widens them.
ld=abcdefghijklmnopqrstuvwxyz0123456789
label="[$ld]([$ld-]*[$ld])?"
[[ $host =~ ^$label(\.$label)*$ ]] ||
	die "the issuer must name a host of lowercase ASCII DNS labels (got '$issuer')"
port=${listen##*:}
[[ $port =~ ^[0-9]+$ ]] || die "--listen $listen names no port"

# Docker's host-gateway is verified on OrbStack, where k3d's
# host.k3d.internal is not this machine. Docker Desktop is not verified.
if [[ -z $host_address ]]; then
	lane_require_tools docker
	host_address=$(docker run --rm --add-host=probe:host-gateway "$probe_image" \
		getent hosts probe 2>/dev/null | awk '{print $1; exit}')
fi
[[ -n $host_address ]] ||
	die "could not resolve host-gateway with docker. Pass --host-address with the address this machine answers on from inside a container."
[[ $host_address =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] ||
	die "the host address must be IPv4, as the EndpointSlice's addressType is (got '$host_address')"

cat <<-YAML
	# Apply these to the Keycard localdev cluster yourself:
	#
	#     ./local/route.sh | kubectl --context k3d-localdev apply -f -
	#
	# and remove them the same way with \`kubectl delete -f -\`.
	apiVersion: v1
	kind: Service
	metadata:
	  name: $name
	  namespace: $gateway_namespace
	spec:
	  ports:
	    - name: http
	      port: $port
	      targetPort: $port
	      protocol: TCP
	---
	apiVersion: discovery.k8s.io/v1
	kind: EndpointSlice
	metadata:
	  name: $name
	  namespace: $gateway_namespace
	  labels:
	    kubernetes.io/service-name: $name
	addressType: IPv4
	ports:
	  - name: http
	    port: $port
	    protocol: TCP
	endpoints:
	  - addresses: ["$host_address"]
	    conditions:
	      ready: true
	---
	apiVersion: gateway.networking.k8s.io/v1
	kind: HTTPRoute
	metadata:
	  name: $name
	  namespace: $gateway_namespace
	spec:
	  hostnames:
	    - $host
	  parentRefs:
	    - group: gateway.networking.k8s.io
	      kind: Gateway
	      name: $gateway_name
	      namespace: $gateway_namespace
	  rules:
	    - matches:
	        - path:
	            type: PathPrefix
	            value: /
	      backendRefs:
	        - group: ""
	          kind: Service
	          name: $name
	          namespace: $gateway_namespace
	          port: $port
YAML
