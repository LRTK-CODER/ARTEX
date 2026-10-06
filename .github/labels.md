# 라벨 목록

이 저장소에서 쓰는 라벨의 정본이다. 접두사 없는 평면 목록으로 10개 안팎을 유지한다. 라벨을 늘리거나 바꿀 때는 이 표를 먼저 고치고, 실제 라벨은 소유자 확인 후 `gh label` 명령으로 맞춘다.

## 유형

이슈 하나에 하나만 붙인다. 브랜치 type과 짝을 이룬다.

| 라벨 | 브랜치 type | 색 | 설명 |
|---|---|---|---|
| `feature` | `feat` | `0e8a16` | 새로 만들 것 |
| `bug` | `fix` | `d73a4a` | 동작하지 않는 것 |
| `chore` | `chore` | `cfd3d7` | 설정, 의존성, 저장소 관리 |
| `docs` | `docs` | `0075ca` | 문서, 설계 문서 |
| `refactor` | `refactor` | `5319e7` | 동작은 그대로, 구조만 변경 |
| `test` | `test` | `bfd4f2` | 테스트 추가·수정 |
| `ci` | `ci` | `1d76db` | CI 워크플로와 설정 |
| `research` | (없음) | `c5def5` | 구현 전 조사. 결과는 보고서다 |

## 상태

붙어 있는 동안은 구현 워커에게 넘기지 않는다. 풀리면 뗀다.

| 라벨 | 색 | 설명 |
|---|---|---|
| `needs-decision` | `fbca04` | 소유자 결정 대기. 미결 질문이 남아 있다 |
| `needs-design` | `f9d0c4` | 착수 전에 조사가 필요하다 |

## 쓰지 않는 것

| 대신 쓰는 것 | 없앤 라벨 |
|---|---|
| 닫기 사유 `gh issue close --reason "not planned"`, `--duplicate-of N` | `wontfix`, `duplicate`, `invalid` |
| sub-issue(부모 이슈) | `tracking` |
| 의존 관계 `--blocked-by` | `blocked` |
| 파일 경로와 브랜치 이름 | `area:*` |

## 맞추는 명령

없는 라벨을 만들고 있는 라벨의 색·설명을 표에 맞춘다(`--force`). 여러 번 실행해도 결과가 같다.

    gh label create feature        --color 0e8a16 --description "새로 만들 것" --force
    gh label create bug            --color d73a4a --description "동작하지 않는 것" --force
    gh label create chore          --color cfd3d7 --description "설정, 의존성, 저장소 관리" --force
    gh label create docs           --color 0075ca --description "문서, 설계 문서" --force
    gh label create refactor       --color 5319e7 --description "동작은 그대로, 구조만 변경" --force
    gh label create test           --color bfd4f2 --description "테스트 추가·수정" --force
    gh label create ci             --color 1d76db --description "CI 워크플로와 설정" --force
    gh label create research       --color c5def5 --description "구현 전 조사. 결과는 보고서다" --force
    gh label create needs-decision --color fbca04 --description "소유자 결정 대기. 미결 질문이 남아 있다" --force
    gh label create needs-design   --color f9d0c4 --description "착수 전에 조사가 필요하다" --force

표에 없는 라벨은 `gh label list --limit 200`으로 찾아 `gh label delete <이름> --yes`로 지운다.
