#!/bin/sh
set -eu

LC_ALL=C
export LC_ALL
umask 022

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_root"

version=${1:-dev}
output_arg=${2:-release}
targets=${TARGETS:-"linux/amd64 linux/arm64 linux-musl/amd64 linux-musl/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"}
max_binary_bytes=${MAX_BINARY_BYTES:-18874368}
source_date_epoch=${SOURCE_DATE_EPOCH:-0}
plan_only=${RELEASE_PLAN_ONLY:-0}
go_command=${GO:-go}
file_command=${FILE:-file}
readelf_command=${READELF:-readelf}
strings_command=${STRINGS:-strings}
docker_command=${DOCKER:-docker}
tar_command=${TAR:-tar}
mv_command=${MV:-mv}
docker_fallback=${MUSL_DOCKER_FALLBACK:-1}
docker_pull=${MUSL_DOCKER_PULL:-missing}
docker_image_amd64=${MUSL_DOCKER_IMAGE_AMD64:-golang:1.25.0-alpine3.22@sha256:68fc16b7551a4cf71251f343a47ff766ec4fa7c02fbdeb4c5ed2a14c2c23ea08}
docker_image_arm64=${MUSL_DOCKER_IMAGE_ARM64:-golang:1.25.0-alpine3.22@sha256:a8d0fd810f0074ba0d7b1245501e9b1fb017922504c8e5ede3334fb876688029}
musl_marker=autogora-musl-cgo-v1
temporary_root=
work_root=

fail() {
  echo "$*" >&2
  exit 1
}

usage_error() {
  echo "$*" >&2
  exit 2
}

is_command() {
  command -v "$1" >/dev/null 2>&1
}

validate_boolean() {
  case "$2" in
    0|1) ;;
    *) usage_error "$1 must be 0 or 1" ;;
  esac
}

validate_target() {
  case "$1" in
    linux/amd64|linux/arm64|linux-musl/amd64|linux-musl/arm64|darwin/amd64|darwin/arm64|windows/amd64|windows/arm64)
      ;;
    *)
      usage_error "unsupported release target: $1"
      ;;
  esac
}

if ! printf '%s' "$version" | grep -Eq '^[A-Za-z0-9._+-]+$'; then
  usage_error "invalid version: $version"
fi
case "$max_binary_bytes" in
  ""|*[!0-9]*) usage_error "MAX_BINARY_BYTES must be a positive integer" ;;
esac
if [ "$max_binary_bytes" -lt 1 ]; then
  usage_error "MAX_BINARY_BYTES must be a positive integer"
fi
case "$source_date_epoch" in
  ""|*[!0-9]*) usage_error "SOURCE_DATE_EPOCH must be a non-negative integer" ;;
esac
validate_boolean RELEASE_PLAN_ONLY "$plan_only"
validate_boolean MUSL_DOCKER_FALLBACK "$docker_fallback"
case "$docker_pull" in
  always|missing|never) ;;
  *) usage_error "MUSL_DOCKER_PULL must be always, missing, or never" ;;
esac
case "$output_arg" in
  ""|/|.|..) usage_error "refusing unsafe output directory: $output_arg" ;;
esac

needs_musl=0
for target in $targets; do
  validate_target "$target"
  case "$target" in
    linux-musl/*) needs_musl=1 ;;
  esac
done

for required in "$go_command" "$tar_command" "$mv_command" gzip; do
  if ! is_command "$required"; then
    fail "required release command is unavailable: $required"
  fi
done
if [ "$needs_musl" = 1 ]; then
  for required in "$file_command" "$readelf_command" "$strings_command"; do
    if ! is_command "$required"; then
      fail "required musl verification command is unavailable: $required"
    fi
  done
fi
if ! "$tar_command" --help 2>&1 | grep -q -- '--sort'; then
  fail "reproducible release archives require GNU tar (set TAR=gtar when needed)"
fi
if ! "$mv_command" --help 2>&1 | grep -q -- '--no-target-directory'; then
  fail "atomic release publication requires GNU mv with --no-target-directory"
fi
if command -v sha256sum >/dev/null 2>&1; then
  checksum_command=sha256sum
elif command -v shasum >/dev/null 2>&1; then
  checksum_command="shasum -a 256"
else
  fail "sha256sum or shasum is required to create release checksums"
fi

resolve_musl_compiler() {
  requested=
  case "$1" in
    amd64)
      requested=${MUSL_CC_AMD64:-}
      candidates=x86_64-linux-musl-gcc
      ;;
    arm64)
      requested=${MUSL_CC_ARM64:-}
      candidates=aarch64-linux-musl-gcc
      ;;
    *)
      return 1
      ;;
  esac
  if [ -n "$requested" ]; then
    case "$requested" in
      *" "*|*"	"*)
        usage_error "MUSL_CC_$1 must name one executable; use a wrapper for compiler arguments"
        ;;
    esac
    if is_command "$requested"; then
      command -v "$requested"
      return 0
    fi
    return 1
  fi
  for candidate in $candidates; do
    if is_command "$candidate"; then
      command -v "$candidate"
      return 0
    fi
  done
  return 1
}

docker_image_for_arch() {
  case "$1" in
    amd64) printf '%s\n' "$docker_image_amd64" ;;
    arm64) printf '%s\n' "$docker_image_arm64" ;;
    *) return 1 ;;
  esac
}

inspect_local_musl_compiler() {
  compiler=$1
  arch=$2
  if ! compiler_identity=$("$compiler" --version 2>&1); then
    fail "cannot read compiler identity: $compiler"
  fi
  if ! machine=$("$compiler" -dumpmachine 2>&1); then
    fail "cannot read compiler target: $compiler"
  fi
  case "$machine" in
    *-linux-musl*) ;;
    *) fail "musl compiler target must be an explicit *-linux-musl* triple: $machine" ;;
  esac
  case "$arch:$machine" in
    amd64:x86_64*-linux-musl*|amd64:amd64*-linux-musl*|arm64:aarch64*-linux-musl*|arm64:arm64*-linux-musl*)
      ;;
    *) fail "musl compiler target $machine does not match $arch" ;;
  esac
  compiler_identity_line=$(printf '%s\n' "$compiler_identity" | sed -n '1p')
  echo "  compiler identity: $compiler_identity_line; target: $machine" >&2
}

check_docker_plan() {
  arch=$1
  image=$(docker_image_for_arch "$arch")
  if ! "$docker_command" info >/dev/null 2>&1; then
    fail "Docker fallback requested but the Docker daemon is unavailable"
  fi
  image_local=0
  if "$docker_command" image inspect "$image" >/dev/null 2>&1; then
    image_local=1
  fi
  case "$docker_pull:$image_local" in
    never:0)
      fail "Docker image is not available locally and MUSL_DOCKER_PULL=never: $image"
      ;;
    always:*|missing:0)
      if ! "$docker_command" manifest inspect "$image" >/dev/null 2>&1; then
        fail "Docker image is unavailable locally and from its registry: $image"
      fi
      echo "  Docker image is available from its registry; a real build may pull it" >&2
      ;;
    *)
      echo "  Docker image is available locally" >&2
      ;;
  esac
}

plan_musl_builder() {
  arch=$1
  if compiler=$(resolve_musl_compiler "$arch"); then
    inspect_local_musl_compiler "$compiler" "$arch"
    echo "  mode=local-cgo cc=$compiler CGO_ENABLED=1 linkmode=external static"
    return
  fi
  if [ "$docker_fallback" = 1 ] && is_command "$docker_command"; then
    check_docker_plan "$arch"
    image=$(docker_image_for_arch "$arch")
    echo "  mode=docker-cgo image=$image platform=linux/$arch pull=$docker_pull CGO_ENABLED=1 linkmode=external static"
    return
  fi
  fail "no musl C compiler for $arch; set MUSL_CC_$(printf '%s' "$arch" | tr '[:lower:]' '[:upper:]') or enable Docker fallback"
}

if [ "$plan_only" = 1 ]; then
  for target in $targets; do
    target_os=${target%/*}
    goarch=${target#*/}
    echo "planning $target_os/$goarch"
    if [ "$target_os" = linux-musl ]; then
      plan_musl_builder "$goarch"
    else
      echo "  mode=pure-go CGO_ENABLED=0"
    fi
  done
  echo "release plan complete; no binaries were built and size limits were not validated"
  exit 0
fi

output_parent_arg=$(dirname -- "$output_arg")
output_basename=$(basename -- "$output_arg")
case "$output_basename" in
  ""|.|..) usage_error "refusing unsafe output directory: $output_arg" ;;
esac
mkdir -p "$output_parent_arg"
output_parent=$(CDPATH= cd -- "$output_parent_arg" && pwd)
output_dir="$output_parent/$output_basename"
if [ -L "$output_dir" ]; then
  usage_error "refusing symlink output directory: $output_dir"
fi
if [ -e "$output_dir" ]; then
  if [ ! -d "$output_dir" ]; then
    usage_error "output path is not a directory: $output_dir"
  fi
  if find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
    usage_error "output directory must be empty: $output_dir"
  fi
fi

temporary_root=$(mktemp -d "$output_parent/.autogora-release-tmp.XXXXXX")
work_root="$temporary_root/.work"
mkdir -p "$work_root"

cleanup_release_temp() {
  if [ -n "${temporary_root:-}" ] && [ -d "$temporary_root" ]; then
    case "$temporary_root" in
      "$output_parent"/.autogora-release-tmp.*) rm -rf "$temporary_root" ;;
      *) echo "refusing unsafe temporary cleanup: $temporary_root" >&2 ;;
    esac
  fi
}

handle_exit() {
  status=$?
  trap - EXIT HUP INT TERM
  cleanup_release_temp
  exit "$status"
}

handle_signal() {
  status=$1
  trap - EXIT HUP INT TERM
  cleanup_release_temp
  exit "$status"
}

trap handle_exit EXIT
trap 'handle_signal 129' HUP
trap 'handle_signal 130' INT
trap 'handle_signal 143' TERM

json_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

write_musl_overlay() {
  overlay_dir=$1
  virtual_root=$2
  replacement_root=$3
  mkdir -p "$overlay_dir"
  marker_source="$overlay_dir/release_musl_cgo.go"
  overlay_file="$overlay_dir/overlay.json"
  cat >"$marker_source" <<EOF
package main

/*
#include <stddef.h>
__attribute__((used)) static const volatile char autogora_musl_cgo_marker[] = "$musl_marker";
static size_t autogora_musl_cgo_marker_size(void) {
	return sizeof(autogora_musl_cgo_marker) + (size_t)autogora_musl_cgo_marker[0];
}
*/
import "C"

var autogoraMuslCGOMarkerSize = C.autogora_musl_cgo_marker_size()
EOF
  virtual_file=$(json_escape "$virtual_root/cmd/autogora/release_musl_cgo.go")
  replacement_file=$(json_escape "$replacement_root/release_musl_cgo.go")
  printf '{"Replace":{"%s":"%s"}}\n' "$virtual_file" "$replacement_file" >"$overlay_file"
}

verify_static_elf() {
  binary=$1
  arch=$2
  if ! description=$("$file_command" "$binary" 2>&1); then
    fail "file failed while inspecting $binary: $description"
  fi
  printf '  file: %s\n' "$description"
  if ! printf '%s\n' "$description" | grep -Eq 'ELF.*statically linked'; then
    fail "musl artifact is not a static ELF binary: $description"
  fi
  if printf '%s\n' "$description" | grep -Eqi 'dynamically linked|interpreter'; then
    fail "musl artifact unexpectedly contains a dynamic loader: $description"
  fi
  if ! header=$("$readelf_command" -h "$binary" 2>&1); then
    fail "readelf -h failed while inspecting $binary: $header"
  fi
  case "$arch" in
    amd64)
      printf '%s\n' "$header" | grep -Eq 'Machine:.*(X86-64|Advanced Micro Devices)' ||
        fail "musl artifact machine does not match amd64"
      ;;
    arm64)
      printf '%s\n' "$header" | grep -Eq 'Machine:.*AArch64' ||
        fail "musl artifact machine does not match arm64"
      ;;
  esac
  if ! program_headers=$("$readelf_command" -l "$binary" 2>&1); then
    fail "readelf -l failed while inspecting $binary: $program_headers"
  fi
  if printf '%s\n' "$program_headers" | grep -q 'INTERP'; then
    fail "musl artifact has an ELF interpreter"
  fi
  if ! dynamic_section=$("$readelf_command" -d "$binary" 2>&1); then
    fail "readelf -d failed while inspecting $binary: $dynamic_section"
  fi
  if printf '%s\n' "$dynamic_section" | grep -q 'NEEDED'; then
    fail "musl artifact has dynamic library dependencies"
  fi
}

scan_binary_strings() {
  binary=$1
  require_marker=$2
  context=$3
  strings_dump=$(mktemp "$work_root/string-scan.XXXXXX")
  if ! "$strings_command" "$binary" >"$strings_dump" 2>&1; then
    rm -f "$strings_dump"
    fail "strings failed while inspecting $context"
  fi
  if [ "$require_marker" = 1 ] && ! grep -Fq "$musl_marker" "$strings_dump"; then
    rm -f "$strings_dump"
    fail "musl artifact is missing the linked C marker"
  fi
  if grep -Fq 'GLIBC_' "$strings_dump"; then
    rm -f "$strings_dump"
    fail "$context contains a GLIBC_ marker"
  fi
  rm -f "$strings_dump"
}

verify_musl_cgo_binary() {
  binary=$1
  arch=$2
  verify_static_elf "$binary" "$arch"
  scan_binary_strings "$binary" 1 "musl artifact"
}

validate_local_musl_compiler() {
  compiler=$1
  arch=$2
  inspect_local_musl_compiler "$compiler" "$arch"
  probe_dir="$work_root/compiler-probe-$arch"
  mkdir -p "$probe_dir"
  cat >"$probe_dir/probe.c" <<'EOF'
#include <stddef.h>
static const char autogora_musl_probe[] = "autogora-musl-probe";
int main(void) { return autogora_musl_probe[0] == 'a' ? 0 : 1; }
EOF
  "$compiler" -static -Os -o "$probe_dir/probe" "$probe_dir/probe.c"
  verify_static_elf "$probe_dir/probe" "$arch"
  scan_binary_strings "$probe_dir/probe" 0 "musl compiler probe"
}

build_pure_go() {
  goos=$1
  goarch=$2
  output=$3
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" SOURCE_DATE_EPOCH="$source_date_epoch" \
    "$go_command" build \
    -trimpath \
    -buildvcs=false \
    -gcflags "github.com/nn1a/autogora/internal/...=-l" \
    -gcflags "github.com/charmbracelet/...=-l" \
    -gcflags "github.com/modelcontextprotocol/...=-l" \
    -gcflags "github.com/google/jsonschema-go/...=-l" \
    -ldflags "-s -w -buildid= -X main.version=$version" \
    -o "$output" \
    ./cmd/autogora
}

build_local_musl() {
  goarch=$1
  output=$2
  compiler=$3
  validate_local_musl_compiler "$compiler" "$goarch"
  overlay_dir="$work_root/local-overlay-$goarch"
  write_musl_overlay "$overlay_dir" "$project_root" "$overlay_dir"
  CC="$compiler" CGO_ENABLED=1 GOOS=linux GOARCH="$goarch" \
    SOURCE_DATE_EPOCH="$source_date_epoch" \
    "$go_command" build \
    -overlay "$overlay_dir/overlay.json" \
    -trimpath \
    -buildvcs=false \
    -gcflags "github.com/nn1a/autogora/internal/...=-l" \
    -gcflags "github.com/charmbracelet/...=-l" \
    -gcflags "github.com/modelcontextprotocol/...=-l" \
    -gcflags "github.com/google/jsonschema-go/...=-l" \
    -ldflags "-s -w -buildid= -linkmode=external -extldflags=-static -X main.version=$version" \
    -o "$output" \
    ./cmd/autogora
  verify_musl_cgo_binary "$output" "$goarch"
}

prepare_docker_image() {
  arch=$1
  image=$2
  if ! "$docker_command" info >/dev/null 2>&1; then
    fail "Docker fallback requested but the Docker daemon is unavailable"
  fi
  image_local=0
  if "$docker_command" image inspect "$image" >/dev/null 2>&1; then
    image_local=1
  fi
  case "$docker_pull:$image_local" in
    always:*)
      "$docker_command" pull --platform "linux/$arch" "$image" >&2
      ;;
    missing:0)
      "$docker_command" pull --platform "linux/$arch" "$image" >&2
      ;;
    never:0)
      fail "Docker image is not available locally and MUSL_DOCKER_PULL=never: $image"
      ;;
  esac
}

build_docker_musl() {
  goarch=$1
  output=$2
  image=$(docker_image_for_arch "$goarch")
  prepare_docker_image "$goarch" "$image"
  overlay_dir="$work_root/docker-overlay-$goarch"
  write_musl_overlay "$overlay_dir" /src /work/overlay
  output_name=$(basename "$output")
  "$docker_command" run --rm --pull=never --platform "linux/$goarch" \
    --mount "type=bind,source=$project_root,target=/src,readonly" \
    --mount "type=bind,source=$overlay_dir,target=/work/overlay,readonly" \
    --mount "type=volume,destination=/work/cache,volume-nocopy" \
    -w /src \
    -e "AUTOGORA_RELEASE_VERSION=$version" \
    -e "AUTOGORA_RELEASE_ARCH=$goarch" \
    -e "AUTOGORA_RELEASE_OUTPUT=$output_name" \
    -e "SOURCE_DATE_EPOCH=$source_date_epoch" \
    -e GOCACHE=/work/cache/go-build \
    -e GOMODCACHE=/work/cache/go-mod \
    "$image" sh -ceu '
      umask 022
      apk add --no-cache \
        gcc=14.2.0-r6 \
        musl-dev=1.2.5-r12 \
        binutils=2.44-r3 >&2
      musl_identity=$(ldd --version 2>&1 || true)
      [ -n "$musl_identity" ]
      printf "%s\n" "$musl_identity" | grep -qi musl
      echo "  Docker toolchain identities:" >&2
      go version >&2
      gcc --version | sed -n "1p" >&2
      printf "  gcc target: %s\n" "$(gcc -dumpmachine)" >&2
      ld --version | sed -n "1p" >&2
      printf "%s\n" "$musl_identity" | sed -n "1,2p" >&2
      CC=gcc CGO_ENABLED=1 GOOS=linux GOARCH="$AUTOGORA_RELEASE_ARCH" \
        go build \
        -overlay /work/overlay/overlay.json \
        -trimpath \
        -buildvcs=false \
        -gcflags "github.com/nn1a/autogora/internal/...=-l" \
        -gcflags "github.com/charmbracelet/...=-l" \
        -gcflags "github.com/modelcontextprotocol/...=-l" \
        -gcflags "github.com/google/jsonschema-go/...=-l" \
        -ldflags "-s -w -buildid= -linkmode=external -extldflags=-static -X main.version=$AUTOGORA_RELEASE_VERSION" \
        -o "/work/$AUTOGORA_RELEASE_OUTPUT" \
        ./cmd/autogora
      chmod 0755 "/work/$AUTOGORA_RELEASE_OUTPUT"
      cat "/work/$AUTOGORA_RELEASE_OUTPUT"
    ' >"$output"
  chmod 0755 "$output"
  verify_musl_cgo_binary "$output" "$goarch"
}

build_musl() {
  goarch=$1
  output=$2
  if compiler=$(resolve_musl_compiler "$goarch"); then
    echo "  using local musl compiler: $compiler"
    build_local_musl "$goarch" "$output" "$compiler"
    return
  fi
  if [ "$docker_fallback" = 1 ] && is_command "$docker_command"; then
    echo "  using Docker musl builder: $(docker_image_for_arch "$goarch")"
    build_docker_musl "$goarch" "$output"
    return
  fi
  fail "no usable musl builder for $goarch"
}

for target in $targets; do
  target_os=${target%/*}
  goarch=${target#*/}
  goos=$target_os
  platform_name=$target_os
  if [ "$target_os" = linux-musl ]; then
    goos=linux
    platform_name=linux_musl
  fi
  archive_name="autogora_${version}_${platform_name}_${goarch}"
  stage_dir="$work_root/$archive_name"
  binary_name=autogora
  if [ "$goos" = windows ]; then
    binary_name=autogora.exe
  fi

  mkdir -p "$stage_dir"
  echo "building $target_os/$goarch"
  if [ "$target_os" = linux-musl ]; then
    build_musl "$goarch" "$stage_dir/$binary_name"
  else
    build_pure_go "$goos" "$goarch" "$stage_dir/$binary_name"
  fi

  binary_bytes=$(wc -c <"$stage_dir/$binary_name")
  if [ "$binary_bytes" -gt "$max_binary_bytes" ]; then
    fail "$target binary exceeds $max_binary_bytes bytes: $binary_bytes"
  fi
  echo "  binary size: $binary_bytes bytes"

  cp README.md "$stage_dir/"
  cp -R docs examples skills "$stage_dir/"
  find "$stage_dir" -type d -exec chmod 0755 {} +
  find "$stage_dir" -type f -exec chmod 0644 {} +
  chmod 0755 "$stage_dir/$binary_name"

  tar_path="$work_root/$archive_name.tar"
  "$tar_command" \
    --sort=name \
    --mtime="@$source_date_epoch" \
    --owner=0 \
    --group=0 \
    --numeric-owner \
    -C "$work_root" \
    -cf "$tar_path" \
    "$archive_name"
  gzip -9n "$tar_path"
  "$mv_command" "$tar_path.gz" "$temporary_root/$archive_name.tar.gz"
  chmod 0644 "$temporary_root/$archive_name.tar.gz"
done

(
  cd "$temporary_root"
  if [ "$checksum_command" = sha256sum ]; then
    sha256sum autogora_*.tar.gz >checksums.txt
  else
    shasum -a 256 autogora_*.tar.gz >checksums.txt
  fi
)
chmod 0644 "$temporary_root/checksums.txt"
rm -rf "$work_root"
work_root=
chmod 0755 "$temporary_root"

if [ -L "$output_dir" ]; then
  fail "output path became a symlink before publication: $output_dir"
fi
if [ -e "$output_dir" ]; then
  if [ ! -d "$output_dir" ] ||
    find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
    fail "output path changed before publication: $output_dir"
  fi
fi
"$mv_command" --no-target-directory "$temporary_root" "$output_dir"
temporary_root=
trap - EXIT HUP INT TERM

echo "release assets written to $output_dir"
