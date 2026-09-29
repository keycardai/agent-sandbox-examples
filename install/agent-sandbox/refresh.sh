#!/usr/bin/env bash
# Downloads the agent-sandbox release manifest that install.sh applies, and
# writes it beside this script. It refuses the download unless its sha256
# equals the digest GitHub publishes for the release asset.
#
# To move to another release, change the version below, run this script, and
# update the sha256 that internal/chart/installsh_test.go pins.
#
# Usage: refresh.sh
set -euo pipefail

version=v1.0.4
repo=kubernetes-sigs/agent-sandbox
asset=sandbox-with-extensions.yaml

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

die() {
	echo "refresh.sh: $*" >&2
	exit 1
}

case ${1-} in
"") ;;
-h | --help) awk 'NR > 1 && !/^#/ { exit } NR > 1 { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; exit 0 ;;
*) die "unknown argument $1. --help lists them." ;;
esac

# shellcheck source=scripts/pinned
. "$here/../../scripts/pinned"
jq=$(pinned_path jq) || exit 1
for tool in curl shasum; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is not installed"
done

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

release=$(curl -fsSL "https://api.github.com/repos/$repo/releases/tags/$version") ||
	die "cannot read release $version of $repo from GitHub"
tag=$("$jq" -r '.tag_name' <<<"$release")
[[ $tag == "$version" ]] || die "GitHub answered with release '$tag', not $version"
want=$("$jq" -r --arg asset "$asset" '[.assets[] | select(.name == $asset) | .digest] | if length == 1 then .[0] else "" end' <<<"$release")
[[ $want =~ ^sha256:[0123456789abcdef]{64}$ ]] ||
	die "release $version publishes no single sha256 digest for $asset (got '$want')"

curl -fsSL -o "$tmp/$asset" "https://github.com/$repo/releases/download/$version/$asset" ||
	die "cannot download $asset from release $version"
got=sha256:$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)
[[ $got == "$want" ]] || die "$asset has $got, but GitHub publishes $want"
grep -q "image: registry.k8s.io/agent-sandbox/agent-sandbox-controller:$version\$" "$tmp/$asset" ||
	die "$asset does not run the controller image $version"

mv "$tmp/$asset" "$here/$asset"
echo "refresh.sh: wrote $here/$asset ($version, $got)"
