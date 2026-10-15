#!/usr/bin/env bash
# Builds and packages Hotfix Server into dist/server/out (run from the repo
# root; needs Go, zip and nfpm on PATH):
#
#   server/packaging/release.sh server-v1.0.0     # or a bare version: 0.0.0-ci.42
#
# Per CPU (x86_64, arm64): HotfixServer-v<ver>-linux-<arch>.tar.gz / .deb /
# .rpm, -windows-<arch>.zip, -macos-<arch>.tar.gz, plus a SHA256SUMS file.
# No raw binaries, .exe or .dmg assets: those names are what the desktop apps'
# updaters look for (see the guard tests in linux/ and windows/updater_test.go).
#
# LICENSE_PUBLIC_KEY (base64 Ed25519) is compiled in; with REQUIRE_LICENSE_KEY=1
# a build without it fails instead of producing a non-enforcing build.
set -euo pipefail

tag="${1:?usage: release.sh <server-vX.Y.Z | version>}"
ver="${tag#server-}"
ver="${ver#v}"
key="${LICENSE_PUBLIC_KEY:-}"
if [ -z "$key" ] && [ "${REQUIRE_LICENSE_KEY:-}" = 1 ]; then
  echo "::error::SERVER_LICENSE_PUBLIC_KEY is not set; refusing to release a Hotfix Server build that doesn't enforce licensing." >&2
  exit 1
fi

root=$(pwd)
work="$root/dist/server"
out="$work/out"
rm -rf "$work"
mkdir -p "$out"
ldflags="-s -w -X main.version=$ver -X main.licensePublicKey=$key"

for target in linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*}
  goarch=${target#*/}
  arch=$([ "$goarch" = amd64 ] && echo x86_64 || echo arm64)
  label=$([ "$os" = darwin ] && echo macos || echo "$os")
  name="HotfixServer-v$ver-$label-$arch"
  stage="$work/$name"
  exe=hotfixd$([ "$os" = windows ] && echo .exe || true)
  mkdir -p "$stage"

  (cd server && GOOS=$os GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$stage/$exe" .)
  cp server/LICENSE "$stage/LICENSE.txt"
  cp server/packaging/README.txt "$stage/README.txt"

  if [ "$os" = windows ]; then
    cp server/packaging/install.ps1 server/packaging/uninstall.ps1 "$stage/"
    (cd "$work" && zip -qr "$out/$name.zip" "$name")
  else
    cp server/packaging/install.sh server/packaging/uninstall.sh "$stage/"
    chmod 0755 "$stage/install.sh" "$stage/uninstall.sh" "$stage/$exe"
    tar -C "$work" -czf "$out/$name.tar.gz" "$name"
  fi

  if [ "$os" = linux ]; then
    install -D -m 0755 "$stage/hotfixd" dist/nfpm-server/hotfixd
    for fmt in deb rpm; do
      ARCH=$goarch VERSION=$ver nfpm package -f server/packaging/nfpm.yaml -p "$fmt" -t "$out/$name.$fmt"
    done
    rm -rf dist/nfpm-server
  fi
done

(cd "$out" && sha256sum -- * > "HotfixServer-v$ver-SHA256SUMS")
ls -la "$out"
