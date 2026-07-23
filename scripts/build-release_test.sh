#!/bin/sh
set -eu

LC_ALL=C
export LC_ALL
umask 022

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
release_script="$project_root/scripts/build-release.sh"
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/autogora-release-test.XXXXXX")

cleanup() {
  rm -rf "$temporary_root"
}
trap cleanup EXIT HUP INT TERM

fail() {
  echo "release test failed: $*" >&2
  exit 1
}

assert_file() {
  [ -f "$1" ] || fail "expected file: $1"
}

assert_absent() {
  [ ! -e "$1" ] || fail "expected path to be absent: $1"
}

assert_empty_dir() {
  [ -d "$1" ] || fail "expected directory: $1"
  if find "$1" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
    fail "expected empty directory: $1"
  fi
}

assert_contains() {
  grep -Fq -- "$2" "$1" || fail "expected '$2' in $1"
}

assert_not_contains() {
  if grep -Fq -- "$2" "$1"; then
    fail "did not expect '$2' in $1"
  fi
}

assert_no_release_temp() {
  search_root=$1
  if find "$search_root" -maxdepth 1 -name '.autogora-release-tmp.*' -print -quit | grep -q .; then
    fail "release transaction temporary directory leaked under $search_root"
  fi
}

fixture="$temporary_root/project"
fake_bin="$temporary_root/fake-bin"
mkdir -p \
  "$fixture/scripts" \
  "$fixture/cmd/autogora" \
  "$fixture/docs" \
  "$fixture/examples" \
  "$fixture/skills" \
  "$fake_bin"
cp "$release_script" "$fixture/scripts/build-release.sh"
chmod +x "$fixture/scripts/build-release.sh"
printf '%s\n' "release fixture" >"$fixture/README.md"
printf '%s\n' "documentation" >"$fixture/docs/guide.md"
printf '%s\n' "example" >"$fixture/examples/example.txt"
printf '%s\n' "skill" >"$fixture/skills/SKILL.md"
printf '%s\n' "package main" >"$fixture/cmd/autogora/main.go"
chmod 0700 "$fixture/docs" "$fixture/examples" "$fixture/skills"
chmod 0600 \
  "$fixture/README.md" \
  "$fixture/docs/guide.md" \
  "$fixture/examples/example.txt" \
  "$fixture/skills/SKILL.md"

cat >"$fake_bin/go" <<'EOF'
#!/bin/sh
set -eu

printf 'go|CGO_ENABLED=%s|GOOS=%s|GOARCH=%s|CC=%s|%s\n' \
  "${CGO_ENABLED:-}" "${GOOS:-}" "${GOARCH:-}" "${CC:-}" "$*" >>"${TEST_LOG:?}"
if [ -n "${FAKE_GO_FAIL_ARCH:-}" ] && [ "${GOARCH:-}" = "$FAKE_GO_FAIL_ARCH" ]; then
  echo "injected Go build failure for $GOARCH" >&2
  exit 42
fi

output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then
    shift
    [ "$#" -gt 0 ] || exit 2
    output=$1
  fi
  shift
done
[ -n "$output" ] || exit 2
printf '%s\n' "fake release binary" "autogora-musl-cgo-v1" >"$output"
chmod 0700 "$output"
EOF

cat >"$fake_bin/x86_64-linux-musl-gcc" <<'EOF'
#!/bin/sh
set -eu

case "${1:-}" in
  --version)
    echo "x86_64-linux-musl-gcc (musl toolchain) 1.2.5"
    exit 0
    ;;
  -dumpmachine)
    echo "${FAKE_CC_MACHINE:-x86_64-linux-musl}"
    exit 0
    ;;
esac

printf 'compiler|%s\n' "$*" >>"${TEST_LOG:?}"
output=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then
    shift
    [ "$#" -gt 0 ] || exit 2
    output=$1
  fi
  shift
done
[ -n "$output" ] || exit 2
printf '%s\n' "fake static musl probe" >"$output"
chmod 0700 "$output"
EOF

cat >"$fake_bin/file" <<'EOF'
#!/bin/sh
if [ "${FAKE_FILE_FAIL:-0}" = 1 ]; then
  echo "injected file failure" >&2
  exit 7
fi
echo "$1: ELF 64-bit LSB executable, x86-64, statically linked, stripped"
EOF

cat >"$fake_bin/readelf" <<'EOF'
#!/bin/sh
if [ "${FAKE_READELF_FAIL:-}" = "$1" ]; then
  echo "injected readelf failure for $1" >&2
  exit 7
fi
case "$1" in
  -h)
    echo "  Machine:                           Advanced Micro Devices X86-64"
    ;;
  -l)
    if [ "${FAKE_READELF_INTERP:-0}" = 1 ]; then
      echo "  INTERP"
    else
      echo "There are no program headers requiring a loader."
    fi
    ;;
  -d)
    if [ "${FAKE_READELF_NEEDED:-0}" = 1 ]; then
      echo "  NEEDED Shared library: libc.so"
    else
      echo "There is no dynamic section in this file."
    fi
    ;;
  *)
    exit 2
    ;;
esac
EOF

cat >"$fake_bin/strings" <<'EOF'
#!/bin/sh
is_probe=0
case "$1" in
  *compiler-probe*) is_probe=1 ;;
esac
case "${FAKE_STRINGS_MODE:-valid}" in
  valid)
    echo "autogora-musl-cgo-v1"
    ;;
  missing-marker)
    echo "static binary without release marker"
    ;;
  glibc-private)
    echo "autogora-musl-cgo-v1"
    if [ "$is_probe" = 0 ]; then echo "GLIBC_PRIVATE"; fi
    ;;
  glibc-abi)
    echo "autogora-musl-cgo-v1"
    if [ "$is_probe" = 0 ]; then echo "GLIBC_ABI_DT_RELR"; fi
    ;;
  glibc-probe)
    echo "GLIBC_PRIVATE"
    ;;
  fail)
    echo "injected strings failure" >&2
    exit 7
    ;;
  *)
    exit 2
    ;;
esac
EOF

cat >"$fake_bin/docker" <<'EOF'
#!/bin/sh
set -eu
printf 'docker|%s\n' "$*" >>"${TEST_LOG:?}"
case "${1:-}" in
  info)
    [ "${FAKE_DOCKER_DAEMON:-up}" = up ]
    ;;
  image)
    [ "${2:-}" = inspect ] || exit 2
    [ "${FAKE_DOCKER_LOCAL:-0}" = 1 ]
    ;;
  manifest)
    [ "${2:-}" = inspect ] || exit 2
    [ "${FAKE_DOCKER_REGISTRY:-1}" = 1 ]
    ;;
  pull)
    exit 0
    ;;
  run)
    printf '%s\n' "fake release binary" "autogora-musl-cgo-v1"
    ;;
  *)
    exit 2
    ;;
esac
EOF

chmod +x "$fake_bin"/*

TEST_LOG="$temporary_root/build.log"
export TEST_LOG
script="$fixture/scripts/build-release.sh"
compiler="$fake_bin/x86_64-linux-musl-gcc"
archive_name=autogora_1.2.3_linux_musl_amd64.tar.gz

run_local_musl_release() {
  output_dir=$1
  TARGETS=linux-musl/amd64 \
    GO="$fake_bin/go" \
    FILE="$fake_bin/file" \
    READELF="$fake_bin/readelf" \
    STRINGS="$fake_bin/strings" \
    MUSL_CC_AMD64="$compiler" \
    MUSL_DOCKER_FALLBACK=0 \
    SOURCE_DATE_EPOCH=123456789 \
    "$script" 1.2.3 "$output_dir"
}

: >"$TEST_LOG"
first_output="$temporary_root/first"
(umask 022; run_local_musl_release "$first_output")
assert_file "$first_output/$archive_name"
assert_file "$first_output/checksums.txt"
assert_contains "$TEST_LOG" "compiler|-static -Os"
assert_contains "$TEST_LOG" "go|CGO_ENABLED=1|GOOS=linux|GOARCH=amd64|CC=$compiler|"
assert_contains "$TEST_LOG" "-overlay "
assert_contains "$TEST_LOG" "-trimpath"
assert_contains "$TEST_LOG" "-buildvcs=false"
assert_contains "$TEST_LOG" "-buildid="
assert_contains "$TEST_LOG" "-linkmode=external"
assert_contains "$TEST_LOG" "-extldflags=-static"
[ ! -e "$fixture/cmd/autogora/release_musl_cgo.go" ] ||
  fail "the release-only cgo source polluted the project"
(
  cd "$first_output"
  sha256sum -c checksums.txt >/dev/null
)

: >"$TEST_LOG"
second_output="$temporary_root/second"
mkdir "$second_output"
(umask 077; run_local_musl_release "$second_output")
cmp "$first_output/$archive_name" "$second_output/$archive_name" >/dev/null ||
  fail "caller umask changed the release archive hash"
[ "$(stat -c %a "$second_output/$archive_name")" = 644 ] ||
  fail "release archive mode is not canonical"
archive_listing="$temporary_root/archive-listing.txt"
tar -tvzf "$second_output/$archive_name" >"$archive_listing"
assert_contains "$archive_listing" "drwxr-xr-x"
assert_contains "$archive_listing" "-rwxr-xr-x"
assert_contains "$archive_listing" "-rw-r--r--"
assert_no_release_temp "$temporary_root"

: >"$TEST_LOG"
pure_output="$temporary_root/pure"
TARGETS=linux/amd64 \
  GO="$fake_bin/go" \
  SOURCE_DATE_EPOCH=123456789 \
  "$script" 1.2.3 "$pure_output"
assert_file "$pure_output/autogora_1.2.3_linux_amd64.tar.gz"
assert_contains "$TEST_LOG" "go|CGO_ENABLED=0|GOOS=linux|GOARCH=amd64|CC=|"
assert_not_contains "$TEST_LOG" "-overlay "
assert_not_contains "$TEST_LOG" "-linkmode=external"

local_plan="$temporary_root/local-plan.txt"
TARGETS=linux-musl/amd64 \
  RELEASE_PLAN_ONLY=1 \
  GO="$fake_bin/go" \
  FILE="$fake_bin/file" \
  READELF="$fake_bin/readelf" \
  STRINGS="$fake_bin/strings" \
  MUSL_CC_AMD64="$compiler" \
  MUSL_DOCKER_FALLBACK=0 \
  "$script" 1.2.3 "$temporary_root/unused-local" >"$local_plan"
assert_contains "$local_plan" "mode=local-cgo"
assert_contains "$local_plan" "CGO_ENABLED=1"
assert_contains "$local_plan" "no binaries were built and size limits were not validated"
assert_absent "$temporary_root/unused-local"

: >"$TEST_LOG"
docker_plan="$temporary_root/docker-plan.txt"
TARGETS=linux-musl/arm64 \
  RELEASE_PLAN_ONLY=1 \
  GO="$fake_bin/go" \
  FILE="$fake_bin/file" \
  READELF="$fake_bin/readelf" \
  STRINGS="$fake_bin/strings" \
  MUSL_CC_ARM64="$temporary_root/missing-arm64-musl-cc" \
  DOCKER="$fake_bin/docker" \
  MUSL_DOCKER_FALLBACK=1 \
  MUSL_DOCKER_PULL=missing \
  "$script" 1.2.3 "$temporary_root/unused-docker" >"$docker_plan"
assert_contains "$docker_plan" "mode=docker-cgo"
assert_contains "$docker_plan" "image=golang:1.25.0-alpine3.22@sha256:a8d0fd810f0074ba0d7b1245501e9b1fb017922504c8e5ede3334fb876688029"
assert_contains "$docker_plan" "platform=linux/arm64"
assert_contains "$docker_plan" "pull=missing"
assert_contains "$docker_plan" "CGO_ENABLED=1"
assert_contains "$TEST_LOG" "docker|info"
assert_contains "$TEST_LOG" "docker|image inspect"
assert_contains "$TEST_LOG" "docker|manifest inspect"
assert_absent "$temporary_root/unused-docker"

expect_musl_failure() {
  label=$1
  expected=$2
  setting=$3
  failure_output="$temporary_root/failure-$label"
  failure_log="$temporary_root/failure-$label.log"
  if env \
    "$setting" \
    TEST_LOG="$TEST_LOG" \
    TARGETS=linux-musl/amd64 \
    GO="$fake_bin/go" \
    FILE="$fake_bin/file" \
    READELF="$fake_bin/readelf" \
    STRINGS="$fake_bin/strings" \
    MUSL_CC_AMD64="$compiler" \
    MUSL_DOCKER_FALLBACK=0 \
    "$script" 1.2.3 "$failure_output" >"$failure_log" 2>&1; then
    fail "$label unexpectedly passed"
  fi
  assert_contains "$failure_log" "$expected"
  assert_absent "$failure_output"
  assert_no_release_temp "$temporary_root"
}

expect_musl_failure bad-triple "explicit *-linux-musl* triple" \
  "FAKE_CC_MACHINE=x86_64-linux-gnu"
expect_musl_failure glibc-private "contains a GLIBC_ marker" \
  "FAKE_STRINGS_MODE=glibc-private"
expect_musl_failure glibc-abi "contains a GLIBC_ marker" \
  "FAKE_STRINGS_MODE=glibc-abi"
expect_musl_failure glibc-probe "musl compiler probe contains a GLIBC_ marker" \
  "FAKE_STRINGS_MODE=glibc-probe"
expect_musl_failure missing-marker "missing the linked C marker" \
  "FAKE_STRINGS_MODE=missing-marker"
expect_musl_failure readelf-header "readelf -h failed" \
  "FAKE_READELF_FAIL=-h"
expect_musl_failure readelf-program "readelf -l failed" \
  "FAKE_READELF_FAIL=-l"
expect_musl_failure readelf-dynamic "readelf -d failed" \
  "FAKE_READELF_FAIL=-d"

partial_parent="$temporary_root/partial-parent"
mkdir "$partial_parent"
partial_output="$partial_parent/release"
partial_log="$temporary_root/partial.log"
if FAKE_GO_FAIL_ARCH=arm64 \
  TARGETS="linux/amd64 linux/arm64" \
  GO="$fake_bin/go" \
  "$script" 1.2.3 "$partial_output" >"$partial_log" 2>&1; then
  fail "a multi-target release with a failed final target unexpectedly passed"
fi
assert_absent "$partial_output"
assert_no_release_temp "$partial_parent"

empty_parent="$temporary_root/empty-parent"
empty_output="$empty_parent/release"
mkdir -p "$empty_output"
if FAKE_GO_FAIL_ARCH=arm64 \
  TARGETS="linux/amd64 linux/arm64" \
  GO="$fake_bin/go" \
  "$script" 1.2.3 "$empty_output" >"$temporary_root/empty-failure.log" 2>&1; then
  fail "a failed release unexpectedly replaced an existing empty output"
fi
assert_empty_dir "$empty_output"
assert_no_release_temp "$empty_parent"

nonempty_output="$temporary_root/nonempty"
mkdir "$nonempty_output"
printf '%s\n' keep >"$nonempty_output/existing.txt"
if TARGETS=linux/amd64 GO="$fake_bin/go" \
  "$script" 1.2.3 "$nonempty_output" >"$temporary_root/nonempty.log" 2>&1; then
  fail "a nonempty output directory was accepted"
fi
assert_file "$nonempty_output/existing.txt"

assert_contains "$release_script" 'MAX_BINARY_BYTES:-18874368'
assert_contains "$release_script" 'gcc=14.2.0-r6'
assert_contains "$release_script" 'musl-dev=1.2.5-r12'
assert_contains "$release_script" 'binutils=2.44-r3'
assert_contains "$release_script" 'type=volume,destination=/work/cache,volume-nocopy'
assert_not_contains "$release_script" 'docker-cache-'

real_go=${GO:-go}
if ! command -v "$real_go" >/dev/null 2>&1; then
  fail "real Go command is required for the current-project size test: $real_go"
fi
real_parent="$temporary_root/real-parent"
real_output="$real_parent/release"
mkdir "$real_parent"
TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64" \
  GO="$real_go" \
  SOURCE_DATE_EPOCH=123456789 \
  "$release_script" integration "$real_output"
for archive in \
  autogora_integration_linux_amd64.tar.gz \
  autogora_integration_linux_arm64.tar.gz \
  autogora_integration_darwin_amd64.tar.gz \
  autogora_integration_darwin_arm64.tar.gz \
  autogora_integration_windows_amd64.tar.gz \
  autogora_integration_windows_arm64.tar.gz; do
  assert_file "$real_output/$archive"
done
(
  cd "$real_output"
  sha256sum -c checksums.txt >/dev/null
)
real_extract="$temporary_root/real-extract"
mkdir "$real_extract"
tar -xzf "$real_output/autogora_integration_linux_amd64.tar.gz" -C "$real_extract"
real_binary="$real_extract/autogora_integration_linux_amd64/autogora"
assert_file "$real_binary"
real_binary_bytes=$(wc -c <"$real_binary")
if [ "$real_binary_bytes" -gt 18874368 ]; then
  fail "current-project linux/amd64 binary exceeds the 18 MiB release budget: $real_binary_bytes"
fi
assert_no_release_temp "$real_parent"

verify_real_musl_archive() {
  archive=$1
  arch=$2
  extract_root=$3
  mkdir "$extract_root"
  tar -xzf "$archive" -C "$extract_root"
  archive_base=$(basename "$archive" .tar.gz)
  binary="$extract_root/$archive_base/autogora"
  assert_file "$binary"
  description=$(file "$binary")
  printf '%s\n' "$description" | grep -Eq 'ELF.*statically linked' ||
    fail "Docker musl $arch binary is not statically linked"
  header=$(readelf -h "$binary")
  case "$arch" in
    amd64) printf '%s\n' "$header" | grep -Eq 'Machine:.*(X86-64|Advanced Micro Devices)' ;;
    arm64) printf '%s\n' "$header" | grep -Eq 'Machine:.*AArch64' ;;
  esac || fail "Docker musl binary architecture does not match $arch"
  program_headers=$(readelf -l "$binary")
  if printf '%s\n' "$program_headers" | grep -q INTERP; then
    fail "Docker musl $arch binary has an interpreter"
  fi
  dynamic_section=$(readelf -d "$binary")
  if printf '%s\n' "$dynamic_section" | grep -q NEEDED; then
    fail "Docker musl $arch binary has a dynamic dependency"
  fi
  strings_file="$temporary_root/real-musl-strings-$arch"
  strings "$binary" >"$strings_file"
  grep -Fq autogora-musl-cgo-v1 "$strings_file" ||
    fail "Docker musl $arch binary is missing the cgo marker"
  if grep -Fq GLIBC_ "$strings_file"; then
    fail "Docker musl $arch binary contains a GLIBC_ marker"
  fi
}

case "${RELEASE_TEST_MUSL:-0}" in
  0) ;;
  1)
    for required in docker file readelf strings; do
      command -v "$required" >/dev/null 2>&1 ||
        fail "test-release-musl requires $required"
    done
    musl_parent="$temporary_root/real-musl-parent"
    musl_output="$musl_parent/release"
    mkdir "$musl_parent"
    TARGETS="linux-musl/amd64 linux-musl/arm64" \
      GO="$real_go" \
      MUSL_CC_AMD64="$temporary_root/no-local-amd64-musl-cc" \
      MUSL_CC_ARM64="$temporary_root/no-local-arm64-musl-cc" \
      MUSL_DOCKER_FALLBACK=1 \
      MUSL_DOCKER_PULL="${MUSL_DOCKER_PULL:-missing}" \
      "$release_script" integration "$musl_output"
    (
      cd "$musl_output"
      sha256sum -c checksums.txt >/dev/null
    )
    verify_real_musl_archive \
      "$musl_output/autogora_integration_linux_musl_amd64.tar.gz" \
      amd64 \
      "$temporary_root/real-musl-amd64"
    verify_real_musl_archive \
      "$musl_output/autogora_integration_linux_musl_arm64.tar.gz" \
      arm64 \
      "$temporary_root/real-musl-arm64"
    assert_no_release_temp "$musl_parent"
    current_uid=$(id -u)
    if find "$musl_output" ! -user "$current_uid" -print -quit | grep -q .; then
      fail "Docker musl release produced output not owned by the invoking user"
    fi
    ;;
  *)
    fail "RELEASE_TEST_MUSL must be 0 or 1"
    ;;
esac

echo "release build tests passed"
