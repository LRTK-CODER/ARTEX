#!/usr/bin/env bash
# ARTEX cross-platform release builder.
#
# The default mode builds one target and embeds the already-exported frontend.
# `./build.sh --release` builds and packages all supported desktop/server targets.
#
# Environment variables:
#   ARTEX_TARGET_OS=linux             One target OS in single-target mode.
#   ARTEX_TARGET_ARCH=amd64           One target arch in single-target mode.
#   ARTEX_TARGETS=linux/amd64,...     Comma-separated targets for multi-target mode.
#   ARTEX_BUILD_VERSION=v0.3.3        Version embedded in the binary and archive name.
#   ARTEX_OUTPUT=/path/to/artex       Explicit binary path in single-target mode.
#   ARTEX_OUTPUT_DIR=dist             Directory for default binary paths.
#   ARTEX_PACKAGE=1                   Create a zip archive for each target.
#   ARTEX_PACKAGE_DIR=dist            Directory for release archives.
#   ARTEX_COMPRESS=off                UPX mode: off, auto, or required.
#   ARTEX_UPX_ARGS="--best --lzma"    Arguments passed to UPX.
#   ARTEX_SKIP_FRONTEND=1             Reuse server/webui/dist (for CI artifact builds).
#   ARTEX_SKIP_NPM_CI=1               Skip npm ci while rebuilding the frontend.
#   ARTEX_GOSUMDB=sum.golang.org      Go checksum database.
set -euo pipefail

cd "$(cd "$(dirname "$0")" && pwd)"

info() { printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok() { printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[!]\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
사용법:
  ./build.sh                         현재 시스템·아키텍처용으로 컴파일한다
  ./build.sh --target linux/amd64   지정한 대상 하나를 컴파일한다
  ./build.sh --release               지원하는 모든 대상을 컴파일하고 패키징한다

옵션:
  --release              Linux, macOS, Windows의 amd64/arm64 대상을 빌드하고 zip을 만든다
  --target OS/ARCH       대상 하나를 정한다. 예: windows/amd64
  --upx                  바이너리를 반드시 UPX로 압축한다(일부 Linux 환경에서 호환성 문제가 생길 수 있다)
  --no-compress          UPX를 쓰지 않고 Go linker로 크기만 줄여 zip으로 압축한다
  --help                 도움말을 보여 준다

여러 대상 목록은 ARTEX_TARGETS로 덮어쓸 수 있다. 예:
  ARTEX_TARGETS=linux/amd64,windows/amd64 ./build.sh --release
EOF
}

RELEASE_TARGETS_DEFAULT="linux/amd64,linux/arm64,darwin/amd64,darwin/arm64,windows/amd64"
ARTEX_RELEASE="${ARTEX_RELEASE:-0}"
ARTEX_COMPRESS="${ARTEX_COMPRESS:-off}"
ARTEX_PACKAGE="${ARTEX_PACKAGE:-0}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --release)
      ARTEX_RELEASE=1
      ARTEX_PACKAGE=1
      shift
      ;;
    --target)
      [ "$#" -ge 2 ] || die "--target에는 OS/ARCH 인자가 필요합니다"
      target_arg="$2"
      case "$target_arg" in
        */*)
          ARTEX_TARGET_OS="${target_arg%%/*}"
          ARTEX_TARGET_ARCH="${target_arg##*/}"
          ARTEX_TARGETS="$target_arg"
          ;;
        *) die "대상은 OS/ARCH 형식이어야 합니다. 예: linux/amd64" ;;
      esac
      shift 2
      ;;
    --no-compress)
      ARTEX_COMPRESS=0
      shift
      ;;
    --upx)
      ARTEX_COMPRESS=required
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *) die "알 수 없는 인자: $1(--help로 사용법을 확인하세요)" ;;
  esac
done

command -v go >/dev/null 2>&1 || die "Go를 찾지 못했습니다(이 프로젝트는 Go 1.26 이상이 필요합니다)"

ARTEX_GOSUMDB="${ARTEX_GOSUMDB:-sum.golang.org}"
if [ -z "${ARTEX_BUILD_VERSION:-}" ]; then
  if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    ARTEX_BUILD_VERSION="$(git describe --tags --always --dirty)"
  else
    ARTEX_BUILD_VERSION="dev"
  fi
fi
# Release tags are commonly passed as v0.3.3; keep the binary version consistent.
ARTEX_BUILD_VERSION="${ARTEX_BUILD_VERSION#v}"
ARTEX_OUTPUT_DIR="${ARTEX_OUTPUT_DIR:-dist}"
ARTEX_PACKAGE_DIR="${ARTEX_PACKAGE_DIR:-$ARTEX_OUTPUT_DIR}"
ARTEX_UPX_ARGS="${ARTEX_UPX_ARGS:---best --lzma}"

if [ "${ARTEX_RELEASE}" = "1" ]; then
  ARTEX_TARGETS="${ARTEX_TARGETS:-$RELEASE_TARGETS_DEFAULT}"
else
  ARTEX_TARGET_OS="${ARTEX_TARGET_OS:-$(GOSUMDB="$ARTEX_GOSUMDB" go env GOOS)}"
  ARTEX_TARGET_ARCH="${ARTEX_TARGET_ARCH:-$(GOSUMDB="$ARTEX_GOSUMDB" go env GOARCH)}"
  ARTEX_TARGETS="${ARTEX_TARGETS:-${ARTEX_TARGET_OS}/${ARTEX_TARGET_ARCH}}"
fi

if [ "${ARTEX_SKIP_FRONTEND:-0}" = "1" ]; then
  [ -d server/webui/dist ] || die "ARTEX_SKIP_FRONTEND=1인데 server/webui/dist가 없습니다"
else
  command -v npm >/dev/null 2>&1 || die "npm을 찾지 못했습니다(프런트엔드 정적 빌드에는 Node.js/npm이 필요합니다)"
  command -v rsync >/dev/null 2>&1 || die "rsync를 찾지 못했습니다"
  info "프런트엔드 정적 리소스를 빌드합니다"
  if [ "${ARTEX_SKIP_NPM_CI:-0}" != "1" ]; then
    (cd web && npm ci)
  fi
  (cd web && npm run build:static)
  info "프런트엔드 리소스를 server/webui/dist에 동기화합니다"
  mkdir -p server/webui/dist
  rsync -a --delete web/out/ server/webui/dist/
fi

compress_binary() {
  binary="$1"
  goos="$2"
  case "$ARTEX_COMPRESS" in
    0|off|false|none)
      info "UPX를 건너뜁니다: $binary"
      return 0
      ;;
    auto|required|true|1) ;;
    *) die "ARTEX_COMPRESS는 off, auto, required 중 하나여야 합니다" ;;
  esac

  if ! command -v upx >/dev/null 2>&1; then
    if [ "$ARTEX_COMPRESS" = "required" ]; then
      die "ARTEX_COMPRESS=required인데 upx를 찾지 못했습니다"
    fi
    warn "upx를 찾지 못해 linker로 줄인 결과를 그대로 둡니다: $binary"
    return 0
  fi

  before=$(wc -c < "$binary" | tr -d ' ')
  upx_args="$ARTEX_UPX_ARGS"
  [ "$goos" = "darwin" ] && upx_args="$upx_args --force-macos"
  # shellcheck disable=SC2086
  if ! upx $upx_args -- "$binary"; then
    if [ "$ARTEX_COMPRESS" = "required" ]; then
      die "UPX로 압축하지 못했습니다: $binary"
    fi
    warn "UPX가 이 대상 형식을 지원하지 않아 압축하지 않은 바이너리를 그대로 둡니다: $binary"
    return 0
  fi
  after=$(wc -c < "$binary" | tr -d ' ')
  ok "UPX 압축을 마쳤습니다: $binary (${before} -> ${after} bytes)"
}

package_binary() {
  binary="$1"
  goos="$2"
  goarch="$3"
  package_name="artex-${ARTEX_BUILD_VERSION}-${goos}-${goarch}"
  package_root="${ARTEX_PACKAGE_DIR}/${package_name}"
  archive="${ARTEX_PACKAGE_DIR}/${package_name}.zip"

  command -v zip >/dev/null 2>&1 || die "패키징에는 zip이 필요합니다"
  rm -rf "$package_root" "$archive"
  mkdir -p "$package_root"
  cp "$binary" "$package_root/"
  # 감시 시작 스크립트가 정식 진입점이다. 화면의 원클릭 업데이트는 프로세스가 끝난 뒤 이 스크립트가 다시 띄워 줘야 하므로
  # artex 본체를 직접 실행하면 업데이트 뒤 다시 뜨지 않는다. 대상 시스템에 맞는 스크립트 하나만 넣는다.
  if [ "$goos" = "windows" ]; then
    cp start.bat "$package_root/"
  else
    cp start.sh "$package_root/"
    chmod +x "$package_root/start.sh"
  fi
  cp -R skills "$package_root/"
  cp config.example.json "$package_root/"
  if [ -f README.md ]; then cp README.md "$package_root/"; fi
  (cd "$ARTEX_PACKAGE_DIR" && zip -q -r -9 "$(basename "$archive")" "$(basename "$package_root")")
  rm -rf "$package_root"
  ok "Release 압축 파일: $archive"
}

build_target() {
  target="$1"
  case "$target" in
    */*) ;;
    *) die "잘못된 대상입니다: $target(OS/ARCH 형식이어야 합니다)" ;;
  esac
  goos="${target%%/*}"
  goarch="${target##*/}"
  case "$goos" in
    linux|darwin|windows) ;;
    *) die "지원하지 않는 시스템입니다: $goos(linux, darwin, windows를 지원합니다)" ;;
  esac

  binary_name="artex"
  [ "$goos" = "windows" ] && binary_name="artex.exe"
  if [ -n "${ARTEX_OUTPUT:-}" ] && [ "$ARTEX_RELEASE" != "1" ]; then
    output="$ARTEX_OUTPUT"
  else
    output="${ARTEX_OUTPUT_DIR}/artex-${goos}-${goarch}/${binary_name}"
  fi
  mkdir -p "$(dirname "$output")"

  info "${goos}/${goarch}를 컴파일합니다. 버전 ${ARTEX_BUILD_VERSION}"
  GOSUMDB="$ARTEX_GOSUMDB" \
  CGO_ENABLED=0 \
  GOOS="$goos" \
  GOARCH="$goarch" \
  go build \
    -tags embedui \
    -trimpath \
    -ldflags "-s -w -buildid= -X main.version=${ARTEX_BUILD_VERSION}" \
    -o "$output" \
    ./cmd/artex

  compress_binary "$output" "$goos"
  if command -v file >/dev/null 2>&1; then file "$output"; fi
  if [ "$ARTEX_PACKAGE" = "1" ]; then package_binary "$output" "$goos" "$goarch"; fi
  ok "컴파일을 마쳤습니다: $output"
}

write_checksums() {
  [ "$ARTEX_PACKAGE" = "1" ] || return 0
  checksum_file="$ARTEX_PACKAGE_DIR/SHA256SUMS"
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$ARTEX_PACKAGE_DIR" && for archive in *.zip; do sha256sum "$archive"; done > "$(basename "$checksum_file")")
  elif command -v shasum >/dev/null 2>&1; then
    (cd "$ARTEX_PACKAGE_DIR" && for archive in *.zip; do shasum -a 256 "$archive"; done > "$(basename "$checksum_file")")
  else
    warn "sha256sum이나 shasum을 찾지 못해 SHA256SUMS를 건너뜁니다"
    return 0
  fi
  ok "체크섬 파일: $checksum_file"
}

mkdir -p "$ARTEX_OUTPUT_DIR"
if [ "$ARTEX_PACKAGE" = "1" ]; then mkdir -p "$ARTEX_PACKAGE_DIR"; fi

old_ifs="$IFS"
IFS=','
read -r -a targets <<< "$ARTEX_TARGETS"
IFS="$old_ifs"
[ "${#targets[@]}" -gt 0 ] || die "ARTEX_TARGETS는 비워 둘 수 없습니다"
for target in "${targets[@]}"; do
  target="${target//[[:space:]]/}"
  [ -n "$target" ] || continue
  build_target "$target"
done

if [ "$ARTEX_PACKAGE" = "1" ]; then
  write_checksums
  info "Release 패키지를 만들었습니다: $ARTEX_PACKAGE_DIR"
fi
