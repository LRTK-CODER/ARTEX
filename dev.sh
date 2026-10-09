#!/usr/bin/env bash
# 개발 모드: 백엔드(:8787) + 트래픽 프록시(:8788)와 프런트엔드 next dev(:5173)를 함께 실행한다.
# 프런트엔드의 /api는 백엔드로 리버스 프록시한다. Ctrl-C로 모두 함께 끝난다.
#
# 단일 바이너리(프런트엔드 내장) 방식은 README의 '단일 바이너리' 절에 있고, 이 스크립트를 쓰지 않는다.
set -euo pipefail
cd "$(dirname "$0")"

# 끝날 때 이 프로세스 그룹의 하위 프로세스(백엔드 + 프런트엔드)를 모두 끝낸다.
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# 백엔드(일반 go run, 프런트엔드를 내장하지 않는다). 동시에 도는 워커 에이전트 수는 '시스템 설정'에서 정한다.
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788 &

# 프런트엔드 핫 리로드(Vite/Next dev server, /api는 :8787로 리버스 프록시).
( cd web && npm run dev ) &

echo "[dev] 백엔드 :8787 / 프록시 :8788 / 프런트엔드 http://localhost:5173  (Ctrl-C로 종료)"
wait
