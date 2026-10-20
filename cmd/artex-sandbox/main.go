// Command artex-sandbox 는 ARTEX 서버가 에이전트 명령을 서버와 다른 uid로 실행할 때 거치는 도우미다(#165).
//
// 서버는 시작할 때 PATH 맨 앞에 이 도우미를 가리키는 "bash" 링크를 둔다. norma는 Bash·백그라운드 작업·
// shell_open에서 PATH의 bash를 실행하므로(norma를 고치지 않는다), 세 경로가 모두 이 도우미를 지나 진짜
// bash로 넘어간다. stdio MCP와 사용자 지정 python 도구는 서버가 "artex-sandbox exec -- <명령>"으로 감싼다.
//
// 도우미는 파일 cap(cap_kill,cap_setuid,cap_setgid)으로 uid를 바꾼다. 모드는 0750 root:<서버 그룹>이라 에이전트 uid는
// 실행하지 못한다. 대상 uid·gid는 환경 변수로 받지만 허용 범위를 코드에 고정해 두어, 서버 uid가 이 도우미로
// root나 다른 시스템 사용자가 될 수 없다.
//
// 사용법:
//
//	bash <bash 인자>                  (argv[0]이 bash인 링크로 실행) 에이전트 uid로 진짜 bash를 실행한다
//	artex-sandbox exec -- <명령> [인자] 에이전트 uid로 명령을 실행한다
//	artex-sandbox check               에이전트 uid로 바꾼 뒤 상태를 한 줄로 찍는다(서버 시작 때 자기 검사)
//	artex-sandbox launch -- <명령> [인자] uid는 그대로 두고 CAP_KILL만 ambient로 올려 명령(서버 start.sh)을 실행한다
//
// launch는 Docker 이미지가 기능을 켰을 때 엔트리포인트에서 쓴다. 서버는 다른 uid가 된 에이전트 프로세스를 죽이려고
// CAP_KILL이 필요하다. 서버 실행 파일에 파일 cap을 주면 커널이 secure-exec로 실행해 Go 런타임이 패닉 추적을 지운다.
// ambient로만 받은 cap은 secure-exec를 일으키지 않는다. Docker는 root가 아닌 사용자에게 ambient cap을 줄 방법이 없어
// 이 도우미의 파일 cap(cap_kill)으로 올린다.
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
