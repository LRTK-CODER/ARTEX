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
  mkdir -p keys && chmod 700 keys

  local cids
  cids="$(docker compose ps -a -q artex)" || die "无法查询 artex 容器（docker compose ps 失败）"
  [ -n "$cids" ] || return 0
  [ "$(printf '%s\n' "$cids" | wc -l)" -eq 1 ] || die "存在多个 artex 容器，无法确定从哪个容器取出服务器密钥"

  local f added
  # docker diff 는 컨테이너 쓰기 층에서 생긴 파일을 보여 준다. 멈춘 컨테이너에도 쓸 수 있고,
  # cp 오류 문구로 "파일 없음"과 "실패"를 가르지 않아도 된다.
  added="$(docker container diff "$cids")" || die "无法检查 artex 容器中的密钥文件（docker container diff 失败）"
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
      die "从 artex 容器取出 /app/$f 失败，已中止（避免丢失密钥）"
    fi
    info "已将服务器密钥 $f 从旧容器移到 ./keys/"
  done
}
