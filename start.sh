#!/bin/sh
# ARTEX 감시 시작 스크립트(Linux / macOS / Docker ENTRYPOINT)
#
# 사용법:
#   ./start.sh                       포그라운드로 실행(Ctrl-C로 중지)
#   nohup ./start.sh >artex.log 2>&1 &   백그라운드에서 계속 실행
#   ./start.sh -addr :9000           추가 인자는 그대로 artex에 넘긴다
#
# 하는 일은 하나다: artex를 실행하고, 프로세스가 끝나면 종료 코드를 보고 다시 띄울지 정한다.
#
#   0      사용자가 정상 중지       → 반복을 끝낸다
#   75     프로그램이 다시 시작 요청 → 바로 다시 실행한다(화면에서 '원클릭 업데이트'나 '롤백'을 눌렀다)
#   그 밖  비정상 종료              → 백오프 뒤 다시 실행한다(1→2→4…최대 60초)
#
# 다운로드, SHA256 검사, 바이너리 교체는 일부러 여기서 하지 않는다. 그 로직을 sh와 bat에 두 벌 써야 하는데,
# 그 부분이야말로 틀리면 안 되는 곳이다. 실행되지 않는 바이너리로 바꿔 버리면 이 스크립트는 그것을
# 계속 다시 띄우고, 사용자는 서버에 접속해 손으로 복구할 수밖에 없다. 그래서 검사·교체는 모두 Go(selfupdate 패키지)에 두고
# artex가 시작할 때 직접 하며, 스크립트는 단순하게 둔다.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] 실행 파일을 찾을 수 없습니다: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# 중지 신호를 artex 본체에 전달한다.
#
# Docker에서는 꼭 필요하다: docker stop은 SIGTERM을 PID 1(곧 이 스크립트)에만 보내고
# 하위 프로세스에는 보내지 않는다. 전달하지 않으면 artex가 신호를 받지 못해 정상 종료를 하지 못하고, 10초 뒤 SIGKILL로
# 강제 종료되어 실행 중인 작업이 중간에 끊긴다.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# 신호가 wait를 끊어 128보다 큰 값을 돌려준다. 이때 하위 프로세스는 아직 정상 종료 중이므로
	# wait를 한 번 더 해야 실제 종료 코드를 얻는다.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] 중지했습니다"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] 정상 종료했습니다"
			exit 0
			;;
		"$RESTART_CODE")
			# 업데이트·롤백이 준비됐다: 다시 실행하면 artex가 시작할 때 바이너리를 교체한다(selfupdate.Bootstrap 참고).
			echo "[artex] 다시 시작을 요청받았습니다. 새 버전을 적용합니다…"
			delay=1
			;;
		*)
			echo "[artex] 비정상 종료했습니다(code=$code). ${delay}초 뒤 다시 시작합니다" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
