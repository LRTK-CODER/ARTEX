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
for cmd in getent groupadd useradd; do
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

created=0
id=$AGENT_POOL_FIRST
while [ "$id" -le "$last" ]; do
  name="artex-agent-$id"
  if ! getent group "$id" >/dev/null; then
    groupadd --gid "$id" "$name"
  fi
  if ! getent passwd "$id" >/dev/null; then
    # --no-log-init: uid가 크면 lastlog·faillog가 큰 희소 파일이 된다. 이 사용자는 로그인하지 않는다.
    useradd --uid "$id" --gid "$id" --no-create-home --home-dir "$AGENT_POOL_HOME" \
      --shell "$AGENT_POOL_SHELL" --no-log-init "$name"
    created=$((created + 1))
  fi
  id=$((id + 1))
done
printf 'agent-pool-users: uid %d-%d 준비됨(새로 만든 사용자 %d개)\n' "$AGENT_POOL_FIRST" "$last" "$created"
