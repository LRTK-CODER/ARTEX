@echo off
rem 콘솔을 UTF-8로 바꾼다. 그러지 않으면 이 파일의 한국어가 CP949 같은 로컬 코드 페이지 터미널에서 깨진다.
chcp 65001 >nul 2>&1
rem ARTEX 감시 시작 스크립트(Windows)
rem
rem 사용법:
rem   start.bat                  포그라운드로 실행(Ctrl-C로 중지)
rem   start.bat -addr :9000      추가 인자는 그대로 artex에 넘긴다
rem
rem 하는 일은 하나다: artex.exe를 실행하고, 프로세스가 끝나면 종료 코드를 보고 다시 띄울지 정한다.
rem
rem   0      사용자가 정상 중지     -> 반복을 끝낸다
rem   75     프로그램이 다시 시작 요청 -> 바로 다시 실행한다(화면에서 '원클릭 업데이트'나 '롤백'을 눌렀다)
rem   그 밖  비정상 종료       -> 백오프 뒤 다시 실행한다(1->2->4…최대 60초)
rem
rem 다운로드, SHA256 검사, 바이너리 교체는 여기서 하지 않고 모두 artex가 시작할 때 직접 한다
rem (selfupdate 패키지). 스크립트는 단순하게 둔다. 자세한 이유는 start.sh 맨 위 설명에 있다.

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] 실행 파일을 찾을 수 없습니다: %BIN% 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] 정상 종료했습니다
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem 업데이트·롤백이 준비됐다: 다시 실행하면 artex가 시작할 때 바이너리를 교체한다.
	echo [artex] 다시 시작을 요청받았습니다. 새 버전을 적용합니다…
	set /a delay=1
	goto loop
)

echo [artex] 비정상 종료했습니다 ^(code=!code!^). !delay!초 뒤 다시 시작합니다 1>&2
rem timeout은 리다이렉트된 콘솔에서 실패하므로 ping으로 대신한다(N초 지연에 N+1회가 필요하다).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
