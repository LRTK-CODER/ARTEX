#!/bin/sh
# 에이전트 uid 풀(#166)의 passwd·group 항목을 만든다. Dockerfile과 install.sh가 함께 쓴다. root로 실행한다.
#
# 에이전트 uid 분리를 켜면 작업·대화형 세션마다 이 풀의 uid 하나를 받는다(gid도 같은 값). passwd 항목이 없는
# uid로는 ssh가 "No user exists for uid"로 실패하고 python getpass 같은 도구도 깨진다. 그래서 풀 전체의 항목을
# 미리 만든다. 셸은 nologin이고 홈은 만들지 않는다(세션별 홈은 #178에서 서버가 준다).
#
# 범위는 db.AgentUIDPoolFirst·AgentUIDPoolSize(db/agent_uids.go)와 같아야 한다. db/agent_uids_test.go가 맞는지 본다.
# #175의 공용 에이전트 사용자 artex-agent(20000)는 풀에 넣지 않는다.
#
# 여러 번 돌려도 같다. 이미 있는 항목(같은 이름, 같은 uid·gid)은 건너뛴다. 다른 이름이 풀의 uid·gid를 쓰고 있거나
# 풀 이름이 다른 uid·gid를 쓰고 있으면 아무것도 고치지 않고 멈춘다.
set -eu

AGENT_POOL_FIRST=20001
AGENT_POOL_SIZE=1024
AGENT_POOL_SHELL=/usr/sbin/nologin
AGENT_POOL_HOME=/nonexistent

die(){ printf 'agent-pool-users: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "root로 실행해야 한다"
for cmd in getent mktemp; do
  command -v "$cmd" >/dev/null 2>&1 || die "$cmd 명령이 없다"
done

last=$((AGENT_POOL_FIRST + AGENT_POOL_SIZE - 1))

# 먼저 전체를 검사하고 나서 만든다. 중간에 멈춰 일부만 만든 상태를 남기지 않기 위해서다.
id=$AGENT_POOL_FIRST
while [ "$id" -le "$last" ]; do
  name="artex-agent-$id"
  by_gid=$(getent group "$id" | cut -d: -f1)
  by_group_name=$(getent group "$name" | cut -d: -f3)
  [ -z "$by_gid" ] || [ "$by_gid" = "$name" ] || die "gid $id를 그룹 $by_gid가 이미 쓴다"
  [ -z "$by_group_name" ] || [ "$by_group_name" = "$id" ] || die "그룹 $name이 gid $by_group_name으로 이미 있다"
  by_uid=$(getent passwd "$id" | cut -d: -f1)
  by_user_name=$(getent passwd "$name" | cut -d: -f3,4)
  [ -z "$by_uid" ] || [ "$by_uid" = "$name" ] || die "uid $id를 사용자 $by_uid가 이미 쓴다"
  [ -z "$by_user_name" ] || [ "$by_user_name" = "$id:$id" ] || die "사용자 $name이 uid:gid $by_user_name으로 이미 있다"
  id=$((id + 1))
done

# 만들기는 한 번에 한다. groupadd·useradd를 1024번씩 부르면 부를 때마다 passwd·shadow·group·gshadow를 통째로
# 다시 써서 이미지 빌드가 수 분 늘어난다. 빠진 항목의 줄을 모아 파일마다 사본에 덧붙이고 rename으로 바꾼다.
# rename이라 읽는 쪽은 옛 파일이나 새 파일 하나만 본다. shadow·gshadow는 있는 시스템에서만 쓴다.
# useradd와 같게 비밀번호는 잠그고(!), lastlog·faillog에는 쓰지 않는다(이 사용자는 로그인하지 않는다).
# shadow 도구의 잠금(lckpwdf)은 잡지 않는다. 이미지 빌드나 설치처럼 다른 계정 변경이 없을 때 돌린다.
work=$(mktemp -d)
trap 'rm -rf "$work"; rm -f /etc/*.artex-agent-pool.$$' EXIT
today=$(( $(date +%s) / 86400 ))
: >"$work/passwd"; : >"$work/group"; : >"$work/shadow"; : >"$work/gshadow"
created=0
id=$AGENT_POOL_FIRST
while [ "$id" -le "$last" ]; do
  name="artex-agent-$id"
  if ! getent group "$id" >/dev/null; then
    printf '%s:x:%d:\n' "$name" "$id" >>"$work/group"
    printf '%s:!::\n' "$name" >>"$work/gshadow"
  fi
  if ! getent passwd "$id" >/dev/null; then
    printf '%s:x:%d:%d::%s:%s\n' "$name" "$id" "$id" "$AGENT_POOL_HOME" "$AGENT_POOL_SHELL" >>"$work/passwd"
    printf '%s:!:%d:0:99999:7:::\n' "$name" "$today" >>"$work/shadow"
    created=$((created + 1))
  fi
  id=$((id + 1))
done

# append_lines 는 /etc/<파일> 의 사본(권한·소유자 유지)에 줄을 덧붙이고 같은 디렉터리 안에서 rename한다.
append_lines(){
  target="/etc/$1"
  [ -s "$work/$1" ] || return 0
  [ -e "$target" ] || return 0
  tmp="$target.artex-agent-pool.$$"
  cp -p "$target" "$tmp"
  # 끝 줄에 줄바꿈이 없으면 덧붙인 첫 줄이 그 줄에 이어 붙는다.
  [ -z "$(tail -c 1 "$tmp")" ] || printf '\n' >>"$tmp"
  cat "$work/$1" >>"$tmp"
  mv -f "$tmp" "$target"
  # SELinux 시스템에서는 새 파일이 /etc 기본 문맥을 받는다. 원래 문맥(shadow_t 등)으로 되돌린다.
  # SELinux가 꺼진 시스템에서는 restorecon이 실패할 수 있으나 그때는 문맥이 필요 없다.
  if command -v restorecon >/dev/null 2>&1; then restorecon "$target" || true; fi
}
# 그룹을 사용자보다 먼저 바꾼다. 중간에 멈춰도 주 그룹이 없는 사용자가 남지 않는다.
append_lines group
append_lines gshadow
append_lines passwd
append_lines shadow
printf 'agent-pool-users: uid %d-%d 준비됨(새로 만든 사용자 %d개)\n' "$AGENT_POOL_FIRST" "$last" "$created"
