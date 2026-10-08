#!/usr/bin/env bash
# The end-to-end checks against a running sandbox. run.sh calls it. It also
# runs on its own against a sandbox that is already up. It does not write to
# any cluster.
#
# Each check prints what it saw. The script fails if any automated check
# fails. Two checks need a person and print what to look for.
#
# Usage: checks.sh --pod NAME --user-id ID [--namespace agents]
#                  [--cluster NAME] [--env FILE]
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# example.com is not an endpoint in sandbox/values.example.yaml.
readonly allowed_url=https://pypi.org/simple/
readonly credentialed_url=https://api.github.com/user
readonly denied_url=https://example.com/

pod=""
user_id=""
namespace=agents
cluster=""
env_file=""

die() {
	echo "checks.sh: $*" >&2
	exit 1
}

need() {
	[[ $1 -ge 2 ]] || die "$2 needs a value"
}

while [[ $# -gt 0 ]]; do
	case $1 in
	--pod) need $# "$1"; pod=$2; shift 2 ;;
	--user-id) need $# "$1"; user_id=$2; shift 2 ;;
	--namespace) need $# "$1"; namespace=$2; shift 2 ;;
	--cluster) need $# "$1"; cluster=$2; shift 2 ;;
	--env) need $# "$1"; env_file=$2; shift 2 ;;
	-h | --help) awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; exit 0 ;;
	*) die "unknown argument $1. --help lists them." ;;
	esac
done

# shellcheck source=local/common.sh
. "$here/common.sh"
load_env "$env_file"
cluster=${cluster:-${KEYCARD_LOCAL_CLUSTER:-}}
github_login=${KEYCARD_LOCAL_GITHUB_LOGIN:-}
require_cluster "$cluster"
[[ -n $pod ]] || die "--pod NAME is required"
[[ -n $user_id ]] || die "--user-id ID is required"
use_pinned_tools kubectl
export KUBERC=off

kube() { kubectl --context "k3d-$cluster" -n "$namespace" "$@"; }
agent() { kube exec "$pod" -c agent -- "$@"; }

failures=0
pass() { echo "  PASS  $1"; }
fail() {
	echo "  FAIL  $1" >&2
	failures=$((failures + 1))
}

# 1. An allowed destination with no credential. The proxy forwards it and
#    does not consult the zone.
status=$(agent curl -sS -o /dev/null -w '%{http_code}' "$allowed_url" 2>/dev/null) || status=""
if [[ $status == 200 ]]; then
	pass "allowed, no credential: $allowed_url returned 200"
else
	fail "allowed, no credential: $allowed_url returned '${status:-a transport error}', want 200"
fi

# 2. The credentialed destination. The agent sends the placeholder. The proxy
#    replaces it with the credential the zone issued for the impersonated user.
login=$(agent sh -c "curl -sS -H \"Authorization: Bearer \$GITHUB_TOKEN\" $credentialed_url" 2>/dev/null |
	sed -n 's/.*"login"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1) || login=""
if [[ -z $login ]]; then
	fail "credentialed: $credentialed_url did not name a GitHub account"
elif [[ -n $github_login && $login != "$github_login" ]]; then
	fail "credentialed: $credentialed_url named '$login', want '$github_login'"
else
	pass "credentialed: $credentialed_url named the GitHub account '$login'"
fi

# 3. A destination no endpoint names. The proxy's resolver answers NXDOMAIN
#    for it, so the client fails at name resolution. The resolver's audit
#    record names the reason.
if agent curl -sS -o /dev/null "$denied_url" >/dev/null 2>&1; then
	fail "denied: $denied_url was reachable"
else
	record=$(kube logs "$pod" -c egress-proxy --tail=500 2>/dev/null | grep -F example.com | tail -1) || record=""
	if [[ -n $record ]]; then
		pass "denied: $denied_url was blocked. The audit record:"
		echo "        $record"
	else
		fail "denied: $denied_url was blocked, but no audit record names it"
	fi
fi

# 4. The pod runs as the ServiceAccount the credential is bound to.
account=$(kube get pod "$pod" -o jsonpath='{.spec.serviceAccountName}') || account=""
if [[ $account == "$user_id" ]]; then
	pass "the pod runs as the ServiceAccount $account"
else
	fail "the pod runs as the ServiceAccount '$account', want '$user_id'"
fi

# keycard-go has no event resource. Read the zone's event log by hand.
cat <<-MSG

	  MANUAL  The zone event log. Expect one credentials:issue for check 2 with
	          subject $user_id and actor system:serviceaccount:$namespace:$user_id,
	          and none for check 3.
	  MANUAL  A sandbox whose document names another enrolled user. Expect the
	          proxy to log the audit reason exchange_denied for its first
	          credentialed request. The zone answers that exchange with
	          access_denied.
MSG

((failures == 0)) || die "$failures check(s) failed"
echo
echo "checks.sh: every automated check passed"
