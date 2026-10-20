# shellcheck shell=bash
# install.sh·update.sh 가 source 하는 Docker 볼륨 준비 함수. 호출하는 스크립트의 info·die 를 쓴다.
#
# 예전 이미지는 서버 키(jwt.key, oauth.key)를 컨테이너 안 /app 에 두었다. 컨테이너를 새 이미지로
# 다시 만들면 그 파일이 사라져 웹 세션과 ChatGPT 구독 로그인이 풀린다. 그래서 다시 만들기 전에
# 지금 컨테이너에서 키를 ./keys(새 이미지의 /app/keys 볼륨)로 꺼내 둔다.

ARTEX_KEY_FILES=(jwt.key oauth.key)

# prepare_key_volume 은 ./keys 를 0700 으로 만들고, 없는 키만 지금 artex 컨테이너에서 꺼낸다.
# 컨테이너가 없거나 컨테이너에 키가 없으면 건너뛴다. 꺼내다 실패하면 die 로 멈춘다.
# 키를 잃은 채 업그레이드를 이어 가는 것보다 멈추는 편이 낫기 때문이다.
prepare_key_volume(){
  mkdir -p keys || die "./keys 디렉터리를 만들지 못했습니다"
  # 이미 다른 소유자(컨테이너 사용자 10001, docker 데몬이 만든 root)면 권한은 prepare_docker_volumes 가 맞춘다.
  if [ -O keys ]; then
    chmod 700 keys || die "./keys 권한을 700으로 바꾸지 못했습니다"
  fi

  local cids
  cids="$(docker compose ps -a -q artex)" || die "artex 컨테이너를 조회하지 못했습니다(docker compose ps 실패)"
  [ -n "$cids" ] || return 0
  [ "$(printf '%s\n' "$cids" | wc -l)" -eq 1 ] || die "artex 컨테이너가 여러 개라 어느 컨테이너에서 서버 키를 꺼낼지 정할 수 없습니다"

  local f added
  # docker diff 는 컨테이너 쓰기 층에서 생긴 파일을 보여 준다. 멈춘 컨테이너에도 쓸 수 있고,
  # cp 오류 문구로 "파일 없음"과 "실패"를 가르지 않아도 된다.
  added="$(docker container diff "$cids")" || die "artex 컨테이너의 키 파일을 확인하지 못했습니다(docker container diff 실패)"
  for f in "${ARTEX_KEY_FILES[@]}"; do
    [ -e "keys/$f" ] && continue
    # 파이프 대신 here-string 을 쓴다. pipefail 아래에서 grep -q 가 일찍 끝나면 printf 가 SIGPIPE 로
    # 실패해 "키 없음"으로 잘못 읽힐 수 있다.
    grep -Fqx -e "A /app/$f" -e "C /app/$f" <<<"$added" || continue
    # 다 꺼낸 뒤에만 제 이름을 붙여, 중간에 실패해도 반쪽짜리 키가 남지 않게 한다.
    if ! { docker cp "$cids:/app/$f" "keys/.$f.tmp" >/dev/null &&
      chmod 600 "keys/.$f.tmp" &&
      mv "keys/.$f.tmp" "keys/$f"; }; then
      rm -f "keys/.$f.tmp"
      die "artex 컨테이너에서 /app/${f}를 꺼내지 못해 중단했습니다(키를 잃지 않으려고 멈춥니다)"
    fi
    info "서버 키 ${f}를 이전 컨테이너에서 ./keys/로 옮겼습니다"
  done
}

# ARTEX_CONTAINER_ID 는 이미지의 artex 사용자 uid·gid 다(Dockerfile 의 ARTEX_UID).
ARTEX_CONTAINER_ID=10001

# prepare_docker_volumes 는 바인드 마운트 소유권을 root 가 아닌 컨테이너 사용자에 맞춘다.
# 예전 이미지는 root 로 돌아 ./data·./keys 가 root(또는 키를 꺼낸 호스트 사용자) 소유다. 그대로 두면 새 이미지가
# 쓰지 못해 시작하지 못한다. sudo 없이 하려고 받은 이미지를 root 로 한 번 띄워 고친다(docker 를 쓸 수 있으면 된다).
#   ./data, ./keys  → 10001:10001 소유. keys 는 0700, 키 파일은 0600
#   ./skills        → 소유자는 호스트 사용자 그대로(git pull 이 고칠 수 있게), 그룹만 10001 로 바꾸고 g+rwX,
#                     디렉터리에 setgid 를 줘 컨테이너가 새로 만든 스킬도 같은 그룹이 되게 한다
# 이미지를 받은 뒤, 컨테이너를 다시 만들기 전에 부른다.
prepare_docker_volumes(){
  mkdir -p data skills || die "./data·./skills 디렉터리를 만들지 못했습니다"
  info "볼륨 소유권을 컨테이너 사용자(uid ${ARTEX_CONTAINER_ID})에 맞춥니다…"
  docker compose run --rm --no-deps --user 0:0 --entrypoint sh artex -c "
    set -e
    # 작업 공간이 크면 chown -R 이 오래 걸린다. 이미 모두 컨테이너 사용자 소유면 건너뛴다.
    if [ -n \"\$(find /app/data /app/keys \\( ! -user ${ARTEX_CONTAINER_ID} -o ! -group ${ARTEX_CONTAINER_ID} \\) -print -quit)\" ]; then
      chown -R ${ARTEX_CONTAINER_ID}:${ARTEX_CONTAINER_ID} /app/data /app/keys
    fi
    chmod 700 /app/keys
    find /app/keys -type f -exec chmod 600 {} +
    chgrp -R ${ARTEX_CONTAINER_ID} /app/skills
    chmod -R g+rwX /app/skills
    find /app/skills -type d -exec chmod g+s {} +
  " || die "볼륨 소유권을 바꾸지 못했습니다. README의 'Docker 볼륨 소유권' 절차를 손으로 따라 하세요"
}
