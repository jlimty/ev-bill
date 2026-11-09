#!/bin/sh
# Runs the pinned Tailwind CSS standalone CLI with the given arguments.
#
# The CLI is a single binary (no Node). On first use this script downloads the
# pinned release for this machine into bin/ (git-ignored) and checks it against
# the SHA-256 recorded below, from the release's own digests. A download that
# fails or does not match stops the script, so a build never runs an
# unverified binary. To update Tailwind, change VERSION and the four digests.
set -eu

VERSION=v4.3.3

case "$(uname -s)-$(uname -m)" in
Darwin-arm64) asset=tailwindcss-macos-arm64 sum=cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d ;;
Darwin-x86_64) asset=tailwindcss-macos-x64 sum=7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1 ;;
Linux-x86_64) asset=tailwindcss-linux-x64 sum=dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a ;;
Linux-aarch64 | Linux-arm64) asset=tailwindcss-linux-arm64 sum=55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195 ;;
*)
	echo "tailwind.sh: no pinned Tailwind binary for $(uname -s) $(uname -m)" >&2
	exit 1
	;;
esac

root=$(cd "$(dirname "$0")/.." && pwd)
bin="$root/bin/$asset-$VERSION"

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

if [ ! -x "$bin" ]; then
	mkdir -p "$root/bin"
	tmp="$bin.download"
	echo "tailwind.sh: downloading $asset $VERSION" >&2
	curl -fsSL --retry 3 -o "$tmp" "https://github.com/tailwindlabs/tailwindcss/releases/download/$VERSION/$asset"
	got=$(sha256 "$tmp")
	if [ "$got" != "$sum" ]; then
		rm -f "$tmp"
		echo "tailwind.sh: $asset $VERSION has SHA-256 $got, want $sum" >&2
		exit 1
	fi
	chmod +x "$tmp"
	mv "$tmp" "$bin"
fi

# The digest is checked on every run, not only after a download, so a binary
# changed in bin/ after it was fetched is refused too.
got=$(sha256 "$bin")
if [ "$got" != "$sum" ]; then
	echo "tailwind.sh: $bin has SHA-256 $got, want $sum; delete it to download again" >&2
	exit 1
fi

exec "$bin" "$@"
