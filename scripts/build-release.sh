#!/bin/sh
set -eu

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"

version=${1:-dev}
output_arg=${2:-release}
targets=${TARGETS:-"linux/amd64 linux/arm64 linux-musl/amd64 linux-musl/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"}
max_binary_bytes=${MAX_BINARY_BYTES:-16777216}

if ! printf '%s' "$version" | grep -Eq '^[A-Za-z0-9._+-]+$'; then
  echo "invalid version: $version" >&2
  exit 2
fi
case "$max_binary_bytes" in
  ""|*[!0-9]*)
    echo "MAX_BINARY_BYTES must be a positive integer" >&2
    exit 2
    ;;
esac
if [ "$max_binary_bytes" -lt 1 ]; then
  echo "MAX_BINARY_BYTES must be a positive integer" >&2
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
  target_os=${target%/*}
  goarch=${target#*/}
  goos=$target_os
  platform_name=$target_os
  if [ "$target_os" = linux-musl ]; then
    goos=linux
    platform_name=linux_musl
  fi
  archive_name="taskcircuit_${version}_${platform_name}_${goarch}"
  stage_dir="$temporary_root/$archive_name"
  binary_name=taskcircuit
  if [ "$goos" = windows ]; then
    binary_name=taskcircuit.exe
  fi

  mkdir -p "$stage_dir"
  echo "building $target_os/$goarch"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
    -trimpath \
    -buildvcs=false \
    -ldflags "-s -w -buildid= -X main.version=$version" \
    -o "$stage_dir/$binary_name" \
    ./cmd/taskcircuit

  binary_bytes=$(wc -c < "$stage_dir/$binary_name")
  if [ "$binary_bytes" -gt "$max_binary_bytes" ]; then
    echo "$target binary exceeds $max_binary_bytes bytes: $binary_bytes" >&2
    exit 1
  fi
  echo "  binary size: $binary_bytes bytes"

  cp README.md "$stage_dir/"
  cp -R docs examples skills "$stage_dir/"
  tar_path="$temporary_root/$archive_name.tar"
  tar -C "$temporary_root" -cf "$tar_path" "$archive_name"
  gzip -9n "$tar_path"
  mv "$tar_path.gz" "$output_dir/$archive_name.tar.gz"
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
