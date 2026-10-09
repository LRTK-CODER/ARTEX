#!/usr/bin/env bash
# ARTEX 설치 스크립트: 1) 모두 Docker로  2) 로컬에서 컴파일해 실행
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }
rand(){ head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24; }
# shellcheck source=docker-keys.sh
. ./docker-keys.sh

# ── docker 환경 확인 ───────────────────
# Docker는 설치하지 않는다. 내려받은 스크립트를 root로 실행하면 검사 없이 패키지 저장소가 등록되므로
# 사용자가 공식 문서대로 직접 설치하게 한다.
ensure_docker(){
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "docker와 docker compose를 찾았다"; return
  fi
  die "docker / docker compose가 없다. 공식 문서대로 설치한 뒤 다시 실행한다: https://docs.docker.com/engine/install/ (macOS·Windows는 Docker Desktop)"
}

# ── 1) 모두 Docker로 ───────────────────────────────
install_docker(){
  ensure_docker
  if [ ! -f .env ]; then
    cp .env.example .env 2>/dev/null || true
    local pw key
    pw="$(ask 'Postgres 비밀번호(Enter를 누르면 무작위로 만듭니다)' "$(rand)")"
    key="$(ask 'ANTHROPIC_API_KEY(비워 두고 나중에 UI에서 설정해도 됩니다)' '')"
    sed -i.bak "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=${pw}|" .env
    sed -i.bak "s|^ANTHROPIC_API_KEY=.*|ANTHROPIC_API_KEY=${key}|" .env
    rm -f .env.bak
    ok ".env를 만들었습니다(POSTGRES_PASSWORD 설정됨)"
  else
    info "이미 있는 .env를 그대로 씁니다"
  fi
  # 서버 키 볼륨(./keys)을 소유자 전용으로 미리 만든다. docker 가 대신 만들면 0755 가 된다.
  # 이미 배포된 곳에서 다시 돌리면 아래 up 이 컨테이너를 새로 만드니, 예전 컨테이너의 키도 꺼내 둔다.
  prepare_key_volume
  info "이미지를 받아 시작합니다…"
  docker compose pull || true
  docker compose up -d
  ok "시작했습니다 → http://localhost:8787"
  info "로그 보기: docker compose logs -f artex"
}

# ── 2) 로컬에서 컴파일해 실행 ──────────────────────────────
install_local(){
  echo "데이터베이스 설치 방식:"
  echo "  1) 이미 있는 PostgreSQL에 연결"
  echo "  2) Docker로 PostgreSQL 띄우기(docker 필요)"
  case "$(ask '선택' 1)" in
    2)
      ensure_docker
      local pw; pw="$(ask 'Postgres 비밀번호(Enter를 누르면 무작위)' "$(rand)")"
      docker run -d --name artex-pg -p 5432:5432 \
        -e POSTGRES_USER=artex -e POSTGRES_PASSWORD="$pw" -e POSTGRES_DB=artex \
        -v artex-pg:/var/lib/postgresql/data postgres:16-alpine
      DB_HOST=127.0.0.1 DB_PORT=5432 DB_USER=artex DB_PASS="$pw" DB_NAME=artex DB_SSL=disable ;;
    *)
      DB_HOST="$(ask '데이터베이스 주소' 127.0.0.1)"
      DB_PORT="$(ask '포트' 5432)"
      DB_USER="$(ask '계정' artex)"
      DB_PASS="$(ask '비밀번호' '')"
      DB_NAME="$(ask '데이터베이스 이름' artex)"
      DB_SSL="$(ask 'sslmode (disable/require)' disable)" ;;
  esac

  # config.json을 만든다
  cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
  ok "config.json을 만들었습니다"

  # go 환경 확인
  command -v go >/dev/null 2>&1 || die "Go를 찾지 못했습니다. 먼저 Go(>=1.26)를 설치하세요: https://go.dev/dl/"
  ok "Go: $(go version)"

  # 프런트엔드를 내장하려면 node로 정적 결과물을 만들어야 한다
  if command -v npm >/dev/null 2>&1; then
    info "프런트엔드 정적 결과물을 빌드합니다…"
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "프런트엔드를 내장한 단일 바이너리를 컴파일합니다…"
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm을 찾지 못했습니다: **프런트엔드를 내장하지 않은** 백엔드를 컴파일합니다(프런트엔드는 npm run dev로 따로 실행하세요)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "컴파일을 마쳤습니다 → ./artex"

  info "시작합니다…(Ctrl-C로 종료)"
  ./artex
}

echo "=============================="
echo "  ARTEX 설치"
echo "  1) 모두 Docker로 설치"
echo "  2) 로컬에서 실행(go 컴파일)"
echo "=============================="
case "$(ask '선택' 1)" in
  1) install_docker ;;
  2) install_local ;;
  *) die "잘못된 선택입니다" ;;
esac
