// Command artex-sandbox 는 ARTEX 서버가 에이전트 명령을 서버와 다른 uid로 실행할 때 거치는 도우미다(#165).
//
// 서버는 시작할 때 PATH 맨 앞에 이 도우미를 가리키는 "bash" 링크를 둔다. norma는 Bash·백그라운드 작업·
// shell_open에서 PATH의 bash를 실행하므로(norma를 고치지 않는다), 세 경로가 모두 이 도우미를 지나 진짜
// bash로 넘어간다. stdio MCP와 사용자 지정 python 도구는 서버가 "artex-sandbox exec -- <명령>"으로 감싼다.
//
// 도우미는 파일 cap(cap_setuid,cap_setgid)으로 uid를 바꾼다. 모드는 0750 root:<서버 그룹>이라 에이전트 uid는
// 실행하지 못한다. 대상 uid·gid는 환경 변수로 받지만 허용 범위를 코드에 고정해 두어, 서버 uid가 이 도우미로
// root나 다른 시스템 사용자가 될 수 없다.
//
// 사용법:
//
//	bash <bash 인자>                  (argv[0]이 bash인 링크로 실행) 에이전트 uid로 진짜 bash를 실행한다
//	artex-sandbox exec -- <명령> [인자] 에이전트 uid로 명령을 실행한다
//	artex-sandbox check               에이전트 uid로 바꾼 뒤 상태를 한 줄로 찍는다(서버 시작 때 자기 검사)
package main

import (
	"fmt"
	"os"
)

// exitFailure 는 도우미 자신이 실패했을 때의 종료 코드다. 셸이 "실행할 수 없음"에 쓰는 값과 같게 둔다.
const exitFailure = 126

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "artex-sandbox: %v\n", err)
		os.Exit(exitFailure)
	}
}
