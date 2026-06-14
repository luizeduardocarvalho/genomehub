#!/bin/sh
# GenomeHub installer (macOS / Linux).
#   curl -fsSL https://raw.githubusercontent.com/luizeduardocarvalho/genomehub/main/install.sh | sh
# Detects OS/arch, downloads the latest release, installs the binary on PATH.
set -e

REPO="luizeduardocarvalho/genomehub"
BIN="genomehub"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*)
		echo "unsupported architecture: $arch" >&2
		exit 1
		;;
esac
case "$os" in
	linux | darwin) ;;
	*)
		echo "unsupported OS: $os (on Windows use install.ps1)" >&2
		exit 1
		;;
esac

echo "resolving latest release..."
tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
	grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name" *: *"([^"]+)".*/\1/')
if [ -z "$tag" ]; then
	echo "could not determine the latest release" >&2
	exit 1
fi
ver=${tag#v}

url="https://github.com/$REPO/releases/download/$tag/${BIN}_${ver}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading $BIN $tag ($os/$arch)..."
curl -fsSL "$url" -o "$tmp/$BIN.tar.gz"
tar -xzf "$tmp/$BIN.tar.gz" -C "$tmp"

# Choose an install dir: /usr/local/bin if writable or via sudo, else ~/.local/bin.
sudo=""
if [ -w /usr/local/bin ]; then
	dst=/usr/local/bin
elif command -v sudo >/dev/null 2>&1; then
	dst=/usr/local/bin
	sudo=sudo
else
	dst="$HOME/.local/bin"
	mkdir -p "$dst"
fi

$sudo install -m 0755 "$tmp/$BIN" "$dst/$BIN"
echo "installed: $dst/$BIN"

case ":$PATH:" in
	*":$dst:"*) ;;
	*) echo "note: add $dst to your PATH to run '$BIN' from anywhere" ;;
esac

"$dst/$BIN" version || true
echo "done. try: $BIN --help"
