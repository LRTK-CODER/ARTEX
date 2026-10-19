#!/usr/bin/env bash
# 사용법: is-latest-tag.sh <이번 태그>  (표준 입력: 저장소의 태그 목록, 한 줄에 하나)
# 이번 태그가 정식 버전(vX.Y.Z) 중 가장 높으면 true, 아니면 false를 출력한다.
# 오래된 태그를 다시 밀어도 Docker 이미지의 latest가 옛 버전으로 돌아가지 않게 하려고 쓴다.
# 시험판(v1.2.0-rc1 등)은 latest 후보가 아니고, 다른 태그와 비교할 때도 뺀다.
# sort -V는 시험판을 정식판보다 뒤에 두고 macOS sort와 동작이 달라 awk로 직접 비교한다.
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "사용법: $0 <이번 태그>  (표준 입력: 태그 목록)" >&2
  exit 2
fi
current="$1"

{ cat; printf '%s\n' "$current"; } | awk -v current="$current" '
  function stable(t) { return t ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/ }
  # 앞자리부터 비교한다. a가 b보다 높으면 1, 같으면 0, 낮으면 -1
  function cmp(a, b,    x, y, i) {
    split(substr(a, 2), x, ".")
    split(substr(b, 2), y, ".")
    for (i = 1; i <= 3; i++) {
      if (x[i] + 0 > y[i] + 0) return 1
      if (x[i] + 0 < y[i] + 0) return -1
    }
    return 0
  }
  stable($0) && (highest == "" || cmp($0, highest) > 0) { highest = $0 }
  END {
    if (stable(current) && cmp(current, highest) == 0) print "true"
    else print "false"
  }
'
