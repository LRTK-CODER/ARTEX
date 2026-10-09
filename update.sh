#!/usr/bin/env bash
# ARTEX 업데이트 스크립트: 1) Docker 업데이트(새 이미지를 받아 다시 만든다)  2) 로컬 컴파일 업데이트(바이너리를 다시 빌드한다)
# install.sh와 짝을 이룬다: install은 처음 설치를, update는 새 버전으로 업그레이드를 맡는다.
# DB 마이그레이션은 손으로 하지 않아도 된다. artex는 시작할 때마다 schema.sql을 멱등하게 다시 실행하므로(ADD COLUMN/CREATE
# INDEX IF NOT EXISTS 포함) 다시 시작하면 마이그레이션된다. 데이터(pgdata 볼륨, ./data, ./keys, ./skills)는 영향을 받지 않는다.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
# shellcheck source=docker-keys.sh
. ./docker-keys.sh

# ── 선택: 저장소를 최신 코드로 맞춘다(compose·스크립트·로컬 컴파일 소스가 모두 이것으로 바뀐다)───────
# $1 은 고른 메뉴 번호다. pull 이 이 스크립트나 docker-keys.sh 를 바꾸면 bash 는 이미 읽은 옛 함수를 그대로
# 돌리므로, 새 update.sh 를 같은 메뉴 번호로 다시 띄운다. ARTEX_UPDATE_SKIP_PULL 이 있으면 pull 을 건너뛰어
# 다시 띄운 스크립트가 또 pull·재실행하지 않는다.
sync_repo(){
  [ -z "${ARTEX_UPDATE_SKIP_PULL:-}" ] || return 0
  if [ ! -d .git ] || ! command -v git >/dev/null 2>&1; then
    warn "git 작업 사본이 아니라 git pull을 건너뜁니다"
    return 0
  fi
  [ "$(ask '최신 코드를 받을까요 (git pull --ff-only)? (y/n)' y)" = y ] || return 0
  local before; before="$(git rev-parse HEAD)"
  if ! git pull --ff-only; then
    warn "git pull을 fast-forward하지 못했습니다(로컬 변경이 있거나 브랜치가 갈라졌습니다). 손으로 정리한 뒤 다시 시도하세요. 이번에는 지금 코드를 그대로 씁니다"
    return 0
  fi
  if ! git diff --quiet "$before" HEAD -- update.sh docker-keys.sh; then
    info "코드와 함께 업데이트 스크립트도 바뀌었습니다. 새 update.sh로 이어서 진행합니다…"
    ARTEX_UPDATE_SKIP_PULL=1 ARTEX_UPDATE_MODE="$1" exec bash ./update.sh
  fi
}

# ── 1) Docker 업데이트 ───────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose를 찾지 못했습니다. 먼저 ./install.sh로 설치하세요"
  [ -f .env ] || die ".env가 없습니다. 먼저 ./install.sh를 실행해 처음 배포를 마치세요"

  # 선택: 지정한 버전 tag로 업그레이드한다(비워 두면 .env의 ARTEX_TAG를 그대로 쓰고, 없으면 latest)
  local tag; tag="$(ask '대상 이미지 tag(Enter를 누르면 .env / latest를 그대로 씁니다)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "ARTEX_TAG를 ${tag}로 설정했습니다"
  fi

  # artex만 바꾼다: postgres는 16-alpine으로 고정이라 함께 업그레이드할 필요가 없다(받으면 대역폭만 낭비하고
  # 메이저 버전이 바뀌면 호환성 위험도 있다). artex가 depends_on postgres를 선언했으므로 서비스 이름을 붙여
  # up하면 pg가 떠 있지 않을 때 자동으로 띄우고, 이미 떠 있으면 그대로 두고 다시 만들지 않는다.
  # 새 이미지로 다시 만들면 예전 컨테이너 안의 키(/app/*.key)가 사라진다. 그 전에 ./keys 로 꺼낸다.
  prepare_key_volume

  info "새 이미지를 받습니다(artex만)…"
  docker compose pull artex
  info "다시 만들어 시작합니다(artex가 다시 시작할 때 schema를 자동으로 마이그레이션합니다)…"
  docker compose up -d artex
  ok "업데이트를 마쳤습니다 → http://localhost:8787"
  info "로그 보기: docker compose logs -f artex"
  info "이전 이미지 정리(선택): docker image prune -f"
}

# ── 2) 로컬 컴파일 업데이트 ──────────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go(>=1.26)를 찾지 못했습니다: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json이 없습니다. 처음 배포라면 ./install.sh를 쓰세요"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "프런트엔드 정적 결과물을 다시 빌드합니다…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "프런트엔드를 내장한 단일 바이너리를 다시 컴파일합니다…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm을 찾지 못했습니다: **프런트엔드를 내장하지 않은** 백엔드를 컴파일합니다(프런트엔드는 npm run dev로 따로 실행하세요)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "컴파일을 마쳤습니다 → ./artex"
  warn "적용하려면 실행 중인 artex 프로세스를 다시 시작하세요(다시 시작할 때 schema를 자동으로 마이그레이션합니다)"
}

# ARTEX_UPDATE_MODE 는 sync_repo 가 새 update.sh 를 다시 띄울 때만 넘긴다. 이미 고른 메뉴를 다시 묻지 않는다.
mode="${ARTEX_UPDATE_MODE:-}"
if [ -z "$mode" ]; then
  echo "=============================="
  echo "  ARTEX 업데이트"
  echo "  1) Docker 업데이트(새 이미지를 받아 다시 만든다)"
  echo "  2) 로컬 업데이트(go로 다시 컴파일)"
  echo "=============================="
  mode="$(ask '선택' 1)"
fi
case "$mode" in
  1) sync_repo 1; update_docker ;;
  2) sync_repo 2; update_local ;;
  *) die "잘못된 선택입니다" ;;
esac
