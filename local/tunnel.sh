#!/usr/bin/env bash
# Publishes the issuer through a Cloudflare tunnel to mirror.sh, so that a
# Keycard zone on the internet can fetch the discovery document and JWKS. It
# runs cloudflared in the foreground until interrupted. The tunnel forwards
# only the issuer's host, to the mirror. The mirror serves only the two
# public documents.
#
# With KEYCARD_LOCAL_TUNNEL empty, it starts a quick tunnel. A quick tunnel
# needs no Cloudflare account, and it gets a new
# https://<name>.trycloudflare.com URL every time it starts. tunnel.sh writes
# that URL to local/out/issuer, and removes the file when it exits. The other
# scripts read the issuer from that file when KEYCARD_LOCAL_ISSUER is empty.
# The cluster's issuer is fixed when the cluster is created, so start
# tunnel.sh before bootstrap.sh. After a restart, delete the cluster with
# down.sh and create it again.
#
# With KEYCARD_LOCAL_TUNNEL set, it runs that named tunnel from your
# Cloudflare account. KEYCARD_LOCAL_ISSUER must be https://<hostname>, and
# the tunnel must have a DNS route for that hostname. docs/local.md lists
# the cloudflared commands that set this up once.
#
# --listen is the mirror's address, 127.0.0.1:8480 unless mirror.sh was
# started with another.
#
# Usage: tunnel.sh [--listen ADDR] [--env FILE]
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

listen=127.0.0.1:8480
env_file=""

die() {
	echo "tunnel.sh: $*" >&2
	exit 1
}

while [[ $# -gt 0 ]]; do
	case $1 in
	--listen) [[ $# -ge 2 ]] || die "--listen needs a value"; listen=$2; shift 2 ;;
	--env) [[ $# -ge 2 ]] || die "--env needs a value"; env_file=$2; shift 2 ;;
	-h | --help) awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; exit 0 ;;
	*) die "unknown argument $1. --help lists them." ;;
	esac
done

# shellcheck source=local/common.sh
. "$here/common.sh"
load_env "$env_file"
tunnel=${KEYCARD_LOCAL_TUNNEL:-}
issuer=${KEYCARD_LOCAL_ISSUER:-}

[[ $listen =~ ^[0-9a-z.-]+:[0-9]+$ ]] || die "--listen must be HOST:PORT (got '$listen')"
if [[ -n $tunnel ]]; then
	# The classes spell out every character so that no locale widens them.
	ld=abcdefghijklmnopqrstuvwxyz0123456789
	label="[$ld]([$ld-]*[$ld])?"
	[[ $issuer =~ ^https://$label(\.$label)+$ ]] ||
		die "the named tunnel $tunnel needs KEYCARD_LOCAL_ISSUER set to https://<hostname>, with no port or path (got '$issuer')"
	[[ $tunnel =~ ^[A-Za-z0-9._-]+$ ]] ||
		die "KEYCARD_LOCAL_TUNNEL must be a tunnel name of letters, digits, dots, dashes and underscores (got '$tunnel')"
else
	[[ -z $issuer ]] ||
		die "KEYCARD_LOCAL_ISSUER is set, but a quick tunnel picks its own URL. Empty KEYCARD_LOCAL_ISSUER, or set KEYCARD_LOCAL_TUNNEL to your named tunnel."
fi

use_pinned_tools cloudflared

if [[ -n $tunnel ]]; then
	echo "tunnel.sh: running the named tunnel $tunnel for $issuer, forwarding to http://$listen"
	exec cloudflared tunnel --no-autoupdate run --url "http://$listen" "$tunnel"
fi

out=$(dirname "$quick_issuer_file")
mkdir -p "$out"
log=$(mktemp)
# A config file of its own keeps a config.yml in ~/.cloudflared from turning
# the quick tunnel into a named one.
config=$(mktemp)
echo 'no-autoupdate: true' >"$config"
pid=""
cleanup() {
	[[ -z $pid ]] || kill "$pid" 2>/dev/null || true
	rm -f "$quick_issuer_file" "$log" "$config"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

cloudflared tunnel --no-autoupdate --config "$config" --url "http://$listen" > >(tee "$log") 2>&1 &
pid=$!

url=""
for _ in {1..60}; do
	# A quick tunnel's name is words joined by dashes. The dash keeps
	# api.trycloudflare.com, which an error can name, from matching.
	url=$(grep -o -E 'https://[a-z0-9]+(-[a-z0-9]+)+\.trycloudflare\.com' "$log" | head -1) || url=""
	[[ -z $url ]] || break
	kill -0 "$pid" 2>/dev/null || die "cloudflared exited before it printed a trycloudflare.com URL"
	sleep 1
done
[[ -n $url ]] || die "cloudflared did not print a trycloudflare.com URL within 60 seconds"
printf '%s\n' "$url" >"$quick_issuer_file"
echo "tunnel.sh: the issuer is $url. It is in $quick_issuer_file until tunnel.sh exits."
wait "$pid"
