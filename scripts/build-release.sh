#!/bin/sh
set -eu

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"

version=${1:-dev}
output_arg=${2:-release}
targets=${TARGETS:-"linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"}

if ! printf '%s' "$version" | grep -Eq '^[A-Za-z0-9._+-]+$'; then
  echo "invalid version: $version" >&2
  exit 2
fi

case "$output_arg" in
  ""|/|.|..)
    echo "refusing unsafe output directory: $output_arg" >&2
    exit 2
    ;;
esac

mkdir -p "$output_arg"
output_dir=$(CDPATH= cd -- "$output_arg" && pwd)
if find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
  echo "output directory must be empty: $output_dir" >&2
  exit 2
fi

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/taskcircuit-release.XXXXXX")
cleanup() {
  rm -r "$temporary_root"
}
trap cleanup EXIT HUP INT TERM

for target in $targets; do
  goos=${target%/*}
  goarch=${target#*/}
  archive_name="taskcircuit_${version}_${goos}_${goarch}"
  stage_dir="$temporary_root/$archive_name"
  binary_name=taskcircuit
  if [ "$goos" = windows ]; then
    binary_name=taskcircuit.exe
  fi

  mkdir -p "$stage_dir"
  echo "building $goos/$goarch"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
    -trimpath \
    -buildvcs=false \
    -ldflags "-s -w -X main.version=$version" \
    -o "$stage_dir/$binary_name" \
    ./cmd/taskcircuit

  cp README.md "$stage_dir/"
  cp -R docs examples skills "$stage_dir/"
  tar -C "$temporary_root" -czf "$output_dir/$archive_name.tar.gz" "$archive_name"
done

(
  cd "$output_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum taskcircuit_*.tar.gz > checksums.txt
  else
    shasum -a 256 taskcircuit_*.tar.gz > checksums.txt
  fi
)

echo "release assets written to $output_dir"
