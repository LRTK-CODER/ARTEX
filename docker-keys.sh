# shellcheck shell=bash
# install.sh·update.sh 가 source 하는 서버 키 볼륨 준비 함수. 호출하는 스크립트의 info·die 를 쓴다.
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
  # docker 데몬이 먼저 만든 ./keys 는 root 소유라 chmod 가 실패한다. 소유자를 바꾸는 방법을 알려 준다.
  chmod 700 keys || die "./keys 권한을 700으로 바꾸지 못했습니다(docker가 root로 만들었을 수 있습니다). sudo chown \"$(id -u):$(id -g)\" keys를 실행한 뒤 다시 시도하세요"

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
