#!/usr/bin/env bash
# =============================================================================
# ARTEX 관리자 비밀번호 재설정 스크립트
#
# 로그인 사용자 이름은 ARTEX로 고정이다. 비밀번호는 bcrypt 해시로 데이터베이스 settings 테이블의
# auth.password_hash 키에 저장된다. 이 스크립트는 데이터베이스에 연결한 뒤 pgcrypto로 DB 안에서 bcrypt 해시를 만들어
# 그 키에 다시 쓴다. 백엔드 로그인 검사(golang.org/x/crypto/bcrypt)와 완전히 호환된다.
#
# 두 가지 배포 방식:
#   local (기본) —— 호스트에서 psql로 데이터베이스에 바로 연결한다. 연결 정보는 다음 우선순위로 얻는다:
#                    명령줄 인자 > --dsn/$ARTEX_PG_DSN > config.json의 database.*
#   docker        —— `docker compose exec`(또는 `docker exec`)로 postgres
#                    컨테이너 안에서 psql을 실행한다(compose는 기본으로 5432를 호스트에 열지 않으므로 컨테이너 안에서 한다).
#
# 사용 예:
#   ./reset-password.sh                          # 로컬. config.json·환경 변수를 자동으로 읽고 새 비밀번호를 대화형으로 입력한다
#   ./reset-password.sh -p 'NewPass!'            # 로컬. 새 비밀번호를 바로 준다
#   ./reset-password.sh --dsn postgres://u:p@h:5432/artex
#   ./reset-password.sh -H 127.0.0.1 -P 5433 -U autopentest -W pass -d artex
#   ./reset-password.sh -m docker                # docker 배포(.env의 POSTGRES_*를 읽는다)
#   ./reset-password.sh -m docker -c <postgres 컨테이너 이름> --exec docker
#
# 보안: 새 비밀번호는 환경 변수 + psql \getenv로 넘기고(프로세스 argv에 들어가지 않는다) :'var'로
# 자동 이스케이프한다(SQL 인젝션 방지). 데이터베이스 비밀번호도 PGPASSWORD로 넘겨 argv에 들어가지 않는다.
# =============================================================================
set -euo pipefail

PASS_KEY="auth.password_hash"
BCRYPT_COST=10

MODE=""            # local | docker(비어 있으면 자동 판정)
DSN=""
HOST="" PORT="" USER="" DBPASS="" DBNAME="" SSLMODE=""
CONFIG=""
CONTAINER=""       # docker 모드의 postgres 서비스·컨테이너 이름(기본값 postgres)
EXEC_KIND=""       # compose | docker(docker 모드에서 쓸 exec 방식. 비어 있으면 자동)
NEWPASS=""
ASSUME_YES=0

die() { echo "오류: $*" >&2; exit 1; }
info() { echo "· $*" >&2; }

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

# ---- 인자 파싱 -------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--mode)        MODE="${2:-}"; shift 2 ;;
    --dsn)            DSN="${2:-}"; shift 2 ;;
    -H|--host)        HOST="${2:-}"; shift 2 ;;
    -P|--port)        PORT="${2:-}"; shift 2 ;;
    -U|--user)        USER="${2:-}"; shift 2 ;;
    -W|--db-password) DBPASS="${2:-}"; shift 2 ;;
    -d|--dbname)      DBNAME="${2:-}"; shift 2 ;;
    --sslmode)        SSLMODE="${2:-}"; shift 2 ;;
    --config)         CONFIG="${2:-}"; shift 2 ;;
    -c|--container)   CONTAINER="${2:-}"; shift 2 ;;
    --exec)           EXEC_KIND="${2:-}"; shift 2 ;;
    -p|--new-password) NEWPASS="${2:-}"; shift 2 ;;
    -y|--yes)         ASSUME_YES=1; shift ;;
    -h|--help)        usage ;;
    *) die "알 수 없는 인자: $1(-h로 사용법을 확인하세요)" ;;
  esac
done

# ---- config.json에서 database.* 읽기(local 모드이고 연결 정보를 직접 주지 않았을 때만)-----
# 안정적인 python3 파싱을 먼저 쓰고, python3이 없으면 grep으로 대신한다(config.json이 필드별로 가지런할 때).
read_config_json() {
  local path="$1"
  [[ -f "$path" ]] || return 1
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1])).get("database", {})
except Exception:
    sys.exit(1)
# dsn을 바로 주거나 필드별로 줄 수 있다
if d.get("dsn"):
    print("DSN\t" + d["dsn"]); sys.exit(0)
for k in ("host","port","user","password","dbname","sslmode"):
    if d.get(k) is not None:
        print(k.upper() + "\t" + str(d[k]))
PY
  else
    # 최소한의 대체 방식: 키마다 grep한다(값은 문자열이나 숫자)
    local k
    for k in host port user password dbname sslmode; do
      local v
      v=$(grep -oE "\"$k\"[[:space:]]*:[[:space:]]*(\"[^\"]*\"|[0-9]+)" "$path" 2>/dev/null \
            | head -1 | sed -E "s/.*:[[:space:]]*//; s/^\"//; s/\"$//") || true
      [[ -n "$v" ]] && echo -e "${k^^}\t$v"
    done
  fi
}

apply_config_fields() {
  local line key val
  while IFS=$'\t' read -r key val; do
    [[ -z "$key" ]] && continue
    case "$key" in
      DSN)      [[ -z "$DSN" ]] && DSN="$val" ;;
      HOST)     [[ -z "$HOST" ]] && HOST="$val" ;;
      PORT)     [[ -z "$PORT" ]] && PORT="$val" ;;
      USER)     [[ -z "$USER" ]] && USER="$val" ;;
      PASSWORD) [[ -z "$DBPASS" ]] && DBPASS="$val" ;;
      DBNAME)   [[ -z "$DBNAME" ]] && DBNAME="$val" ;;
      SSLMODE)  [[ -z "$SSLMODE" ]] && SSLMODE="$val" ;;
    esac
  done
}

# ---- 모드 자동 판정 ---------------------------------------------------------
if [[ -z "$MODE" ]]; then
  if [[ -n "$DSN$HOST$USER$DBNAME" || -n "${ARTEX_PG_DSN:-}" || -f "${CONFIG:-config.json}" ]]; then
    MODE="local"
  elif command -v docker >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
    MODE="docker"
  else
    MODE="local"
  fi
fi
info "배포 모드: $MODE"

# ---- 새 비밀번호 받기 -----------------------------------------------------------
if [[ -z "$NEWPASS" ]]; then
  read -r -s -p "새 비밀번호를 입력하세요(사용자 이름은 ARTEX로 고정): " NEWPASS; echo >&2
  [[ -n "$NEWPASS" ]] || die "비밀번호는 비워 둘 수 없습니다"
  read -r -s -p "확인을 위해 한 번 더 입력하세요: " NEWPASS2; echo >&2
  [[ "$NEWPASS" == "$NEWPASS2" ]] || die "두 번 입력한 비밀번호가 다릅니다"
fi
[[ -n "$NEWPASS" ]] || die "비밀번호는 비워 둘 수 없습니다"

# 환경 변수로 비밀번호를 psql에 넘긴다(\getenv로 읽으므로 argv·ps에 나오지 않는다)
export ARTEX_RESET_NEWPASS="$NEWPASS"

# DB 안에서 bcrypt를 만들고 upsert한다. 비밀번호는 :'newpw'로 자동 이스케이프한다. CREATE EXTENSION은 멱등이지만
# 데이터베이스 역할에 확장을 만들 권한이 없으면 여기서 오류가 난다(안내는 아래 실행 단계의 실패 분기에 있다).
SQL=$(cat <<SQL
\\set ON_ERROR_STOP on
\\getenv newpw ARTEX_RESET_NEWPASS
CREATE EXTENSION IF NOT EXISTS pgcrypto;
INSERT INTO settings(key, value)
VALUES ('$PASS_KEY', crypt(:'newpw', gen_salt('bf', $BCRYPT_COST)))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
SQL
)

# ---- 실행 -----------------------------------------------------------------
if [[ "$MODE" == "local" ]]; then
  # 연결 정보 우선순위: 명령줄 > --dsn/$ARTEX_PG_DSN > config.json
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    [[ -n "${ARTEX_PG_DSN:-}" ]] && DSN="$ARTEX_PG_DSN"
  fi
  if [[ -z "$DSN" && -z "$HOST$USER$DBNAME" ]]; then
    cfg="${CONFIG:-config.json}"
    if [[ -f "$cfg" ]]; then
      info "${cfg}에서 데이터베이스 설정을 읽습니다"
      apply_config_fields < <(read_config_json "$cfg")
    fi
  fi

  command -v psql >/dev/null 2>&1 || die "이 컴퓨터에서 psql을 찾지 못했습니다(postgresql-client를 설치하거나 -m docker를 쓰세요)"

  declare -a PSQL_ARGS=()
  if [[ -n "$DSN" ]]; then
    PSQL_ARGS=("$DSN")
    target="$DSN"
  else
    [[ -n "$USER"   ]] || die "데이터베이스 사용자(-U)나 올바른 config.json/DSN이 없습니다"
    [[ -n "$DBNAME" ]] || die "데이터베이스 이름(-d)이나 올바른 config.json/DSN이 없습니다"
    HOST="${HOST:-127.0.0.1}"; PORT="${PORT:-5432}"; SSLMODE="${SSLMODE:-disable}"
    PSQL_ARGS=(-h "$HOST" -p "$PORT" -U "$USER" -d "$DBNAME")
    [[ -n "$SSLMODE" ]] && export PGSSLMODE="$SSLMODE"
    [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
    target="$USER@$HOST:$PORT/$DBNAME"
  fi

  info "대상 데이터베이스: $target"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "이 데이터베이스에서 ARTEX 비밀번호를 재설정할까요? [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "취소했습니다"
  fi

  if ! printf '%s\n' "$SQL" | psql "${PSQL_ARGS[@]}" -v ON_ERROR_STOP=1 -q >/dev/null; then
    die "비밀번호를 저장하지 못했습니다. pgcrypto 권한이나 확장이 없다는 오류라면 확장을 만들 권한이 있는 역할을 쓰거나, 먼저 CREATE EXTENSION pgcrypto를 직접 실행하세요."
  fi

else
  # ---- docker ----
  command -v docker >/dev/null 2>&1 || die "docker를 찾지 못했습니다"
  CONTAINER="${CONTAINER:-postgres}"

  # exec 방식 고르기: docker compose exec(서비스 이름)를 먼저 쓰고, 안 되면 docker exec(컨테이너 이름)를 쓴다
  if [[ -z "$EXEC_KIND" ]]; then
    if docker compose version >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
      EXEC_KIND="compose"
    else
      EXEC_KIND="docker"
    fi
  fi

  # 컨테이너 안 psql 자격 증명: 명령줄을 먼저, 다음은 .env의 POSTGRES_*, 마지막으로 compose 기본값(artex)
  if [[ -f .env ]]; then
    # shellcheck disable=SC1091
    set -a; . ./.env; set +a
  fi
  DUSER="${USER:-${POSTGRES_USER:-artex}}"
  DNAME="${DBNAME:-${POSTGRES_DB:-artex}}"
  [[ -n "$DBPASS" ]] && export PGPASSWORD="$DBPASS"
  [[ -z "${PGPASSWORD:-}" && -n "${POSTGRES_PASSWORD:-}" ]] && export PGPASSWORD="$POSTGRES_PASSWORD"

  info "대상: 컨테이너 $CONTAINER 안의 psql -U $DUSER -d $DNAME(exec=$EXEC_KIND)"
  if [[ "$ASSUME_YES" -ne 1 ]]; then
    read -r -p "이 컨테이너의 데이터베이스에서 ARTEX 비밀번호를 재설정할까요? [y/N] " ans
    [[ "$ans" == "y" || "$ans" == "Y" ]] || die "취소했습니다"
  fi

  # -e에 값 없이 이름만 주면 현재 환경에서 이어받으므로 비밀번호가 docker 명령 argv에 나오지 않는다.
  declare -a EXEC_CMD
  if [[ "$EXEC_KIND" == "compose" ]]; then
    EXEC_CMD=(docker compose exec -T -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  else
    EXEC_CMD=(docker exec -i -e ARTEX_RESET_NEWPASS -e PGPASSWORD "$CONTAINER"
              psql -U "$DUSER" -d "$DNAME" -v ON_ERROR_STOP=1 -q)
  fi

  if ! printf '%s\n' "$SQL" | "${EXEC_CMD[@]}" >/dev/null; then
    die "비밀번호를 저장하지 못했습니다. 컨테이너 이름(-c), 데이터베이스 계정(.env의 POSTGRES_*), 역할의 pgcrypto 권한을 확인하세요."
  fi
fi

unset ARTEX_RESET_NEWPASS
echo "✓ ARTEX 관리자 비밀번호를 재설정했습니다. 사용자 이름 ARTEX와 새 비밀번호로 로그인하세요(서비스를 다시 시작하지 않아도 됩니다)."
