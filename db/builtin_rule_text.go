package db

import "fmt"

// builtinTextMigration 은 #108 이전에 seed 한 중국어 값(legacy)과 지금 seed 하는 한국어 값(current)의 쌍이다.
type builtinTextMigration struct{ legacy, current string }

// 명령 차단 규칙과 자산 차단 규칙의 seed 는 설정 플래그로 한 번만 돌아, 기존 설치에는 중국어 행이
// 남는다. 아래 표의 legacy 값은 그 행을 찾는 데이터라 번역하지 않는다.
var legacyInterceptRuleNames = []builtinTextMigration{
	{"[内置] 递归强制删除 rm -rf", "[내장] 재귀 강제 삭제 rm -rf"},
	{"[内置] 删除系统关键目录", "[내장] 시스템 핵심 디렉터리 삭제"},
	{"[内置] 磁盘格式化 mkfs", "[내장] 디스크 포맷 mkfs"},
	{"[内置] 覆写磁盘设备 dd", "[내장] 디스크 장치 덮어쓰기 dd"},
	{"[内置] Fork 炸弹", "[내장] Fork 폭탄"},
	{"[内置] 关机 / 重启", "[내장] 시스템 종료 / 재부팅"},
	{"[内置] 杀死全部进程", "[내장] 모든 프로세스 강제 종료"},
	{"[内置] 磁盘擦除 shred / wipe", "[내장] 디스크 완전 삭제 shred / wipe"},
	{"[内置] 清空防火墙规则", "[내장] 방화벽 규칙 비우기"},
	{"[内置] SQL DROP DATABASE / TABLE / SCHEMA", "[내장] SQL DROP DATABASE / TABLE / SCHEMA"},
	{"[内置] SQL TRUNCATE", "[내장] SQL TRUNCATE"},
	{"[内置] MongoDB drop / dropDatabase", "[내장] MongoDB drop / dropDatabase"},
	{"[内置] Redis FLUSHALL / FLUSHDB", "[내장] Redis FLUSHALL / FLUSHDB"},
	{"[内置] curl / wget 发送 DELETE 请求", "[내장] curl / wget DELETE 요청"},
	{"[内置] Python HTTP 客户端 DELETE（requests/httpx/aiohttp）", "[내장] Python HTTP 클라이언트 DELETE(requests/httpx/aiohttp)"},
	{"[内置] 脚本中声明 HTTP DELETE 方法（JS/通用）", "[내장] 스크립트의 HTTP DELETE 메서드 지정(JS/일반)"},
	{"[内置] 批量清空 / 清除接口路径", "[내장] 일괄 비우기 / 파기 API 경로"},
	{"[内置] 破坏性系统命令", "[내장] 시스템 파괴 명령"},
	{"[内置] 数据外泄管道", "[내장] 데이터 유출 파이프"},
	{"[内置] 删除类接口路径", "[내장] 삭제 API 경로"},
}

var legacyInterceptRuleMessages = []builtinTextMigration{
	{"禁止执行递归强制删除（rm -rf / rm --recursive），可能永久损坏系统或靶机环境", "재귀 강제 삭제(rm -rf / rm --recursive)를 금지합니다. 시스템이나 테스트 대상 환경을 영구히 손상시킬 수 있습니다"},
	{"禁止删除系统关键路径", "시스템 핵심 경로 삭제를 금지합니다"},
	{"禁止格式化磁盘（mkfs）", "디스크 포맷(mkfs)을 금지합니다"},
	{"禁止使用 dd 覆写磁盘设备", "dd로 디스크 장치를 덮어쓰는 것을 금지합니다"},
	{"禁止执行 Fork 炸弹", "Fork 폭탄 실행을 금지합니다"},
	{"禁止执行关机或重启命令", "시스템 종료·재부팅 명령 실행을 금지합니다"},
	{"禁止 kill -9 -1 或 killall -9（杀死所有进程）", "kill -9 -1 또는 killall -9(모든 프로세스 강제 종료)를 금지합니다"},
	{"禁止对磁盘设备执行 shred/wipe 擦除", "디스크 장치에 shred/wipe 완전 삭제를 실행하는 것을 금지합니다"},
	{"禁止清空防火墙规则（iptables -F / nft flush）", "방화벽 규칙 비우기(iptables -F / nft flush)를 금지합니다"},
	{"禁止执行 DROP 操作，可能不可逆地销毁数据库对象", "DROP 실행을 금지합니다. 데이터베이스 객체를 되돌릴 수 없게 없앨 수 있습니다"},
	{"禁止执行 TRUNCATE，可能清空数据表所有数据", "TRUNCATE 실행을 금지합니다. 테이블의 모든 데이터를 비울 수 있습니다"},
	{"禁止执行 MongoDB drop 操作", "MongoDB drop 실행을 금지합니다"},
	{"禁止执行 Redis FLUSHALL / FLUSHDB，可能清空全部缓存数据", "Redis FLUSHALL / FLUSHDB 실행을 금지합니다. 캐시 데이터를 모두 비울 수 있습니다"},
	{"禁止通过 curl/wget 发送 HTTP DELETE 请求，可能删除目标系统数据", "curl/wget으로 HTTP DELETE 요청을 보내는 것을 금지합니다. 대상 시스템의 데이터를 삭제할 수 있습니다"},
	{"禁止使用 Python HTTP 客户端发送 DELETE 请求", "Python HTTP 클라이언트로 DELETE 요청을 보내는 것을 금지합니다"},
	{"禁止在脚本中声明并发送 HTTP DELETE 请求", "스크립트에서 HTTP DELETE 메서드를 지정해 요청을 보내는 것을 금지합니다"},
	{"禁止调用批量清空或销毁类接口（/clear /wipe /flush /purge 等）", "일괄 비우기·파기 계열 API(/clear /wipe /flush /purge 등) 호출을 금지합니다"},
	{"破坏性命令被拒绝（rm -rf / / mkfs / dd / fork bomb / 关机重启 / 覆写磁盘设备）", "파괴 명령을 거부했습니다(rm -rf / / mkfs / dd / fork bomb / 시스템 종료·재부팅 / 디스크 장치 덮어쓰기)"},
	{"疑似数据外泄管道被拒绝（命令输出经 curl/wget/nc 外传）", "데이터 유출로 의심되는 파이프를 거부했습니다(명령 출력을 curl/wget/nc로 외부에 보냄)"},
	{"禁止调用删除类接口（/delete /remove /unlink /erase 等），不论使用哪种 HTTP 方法——多数应用的删除接口用 GET/POST 就能触发，同样会真实删除目标数据", "삭제 계열 API(/delete /remove /unlink /erase 등) 호출을 금지합니다. HTTP 메서드와 관계없이 막습니다. 대부분의 애플리케이션은 GET/POST로도 삭제 API가 동작해 대상 데이터를 실제로 삭제합니다"},
}

var legacyAssetInterceptRuleNotes = []builtinTextMigration{
	{"[内置] 政府网站 (.gov)", "[내장] 정부 웹사이트 (.gov)"},
	{"[内置] 政府网站 (.gov.cn)", "[내장] 정부 웹사이트 (.gov.cn)"},
	{"[内置] 教育网站 (.edu)", "[내장] 교육 웹사이트 (.edu)"},
	{"[内置] 教育网站 (.edu.cn)", "[내장] 교육 웹사이트 (.edu.cn)"},
}

// migrateBuiltinRuleTexts 는 기존 설치의 내장 차단 규칙 이름·메시지·메모를 한국어로 바꾼다.
// 값이 이전 seed 와 정확히 같은 필드만 바꾸므로 사용자가 고친 값은 그대로 남는다. intercept_rules 에는
// builtin 열이 없어 이름·메시지 값 자체로 기본 행을 알아본다. 바꾼 뒤에는 legacy 값과 같은 행이 없어
// 매 시작 다시 돌아도 결과가 같다.
func (d *DB) migrateBuiltinRuleTexts() error {
	for _, m := range legacyInterceptRuleNames {
		if _, err := d.Exec(`UPDATE intercept_rules SET name = $1 WHERE name = $2`, m.current, m.legacy); err != nil {
			return fmt.Errorf("차단 규칙 이름 %q 이전: %w", m.current, err)
		}
	}
	for _, m := range legacyInterceptRuleMessages {
		if _, err := d.Exec(`UPDATE intercept_rules SET message = $1 WHERE message = $2`, m.current, m.legacy); err != nil {
			return fmt.Errorf("차단 규칙 메시지 %q 이전: %w", m.current, err)
		}
	}
	for _, m := range legacyAssetInterceptRuleNotes {
		if _, err := d.Exec(`UPDATE asset_intercept_rules SET note = $1 WHERE builtin AND note = $2`, m.current, m.legacy); err != nil {
			return fmt.Errorf("자산 차단 규칙 메모 %q 이전: %w", m.current, err)
		}
	}
	return nil
}
