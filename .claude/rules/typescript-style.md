---
paths: ["web/**/*.ts", "web/**/*.tsx", "web/**/*.js", "web/**/*.jsx", "web/**/*.mjs"]
---

# TypeScript·프런트엔드 규칙

프런트엔드(`web/`) 코드를 쓰거나 고칠 때 따른다. 언어와 무관한 원칙은 `coding-style.md`, 구조는 `architecture.md` "프런트엔드 구조"에 있고 여기서 되풀이하지 않는다. 스택은 Next.js 16(App Router), React 19, TypeScript(strict), Tailwind 4, shadcn/ui다.

## 도구와 문서의 분담

- **형식과 린트는 Biome가 정본이다**(`web/biome.json`). 형식에 대한 리뷰 코멘트는 남기지 않는다. `npm run check:fix`(= `biome check --write`)가 고친다. Edit·Write 뒤에는 PostToolUse hook(`format_code.py`)이 `biome check --write`로 고친다. 커밋 때는 git pre-commit(`.githooks/pre-commit`)이 스테이징된 web 파일에 `biome check`를 돌려 검사만 한다(고치지 않는다). `web/package.json`의 husky 설정과 `web/.husky/pre-commit`은 쓰이지 않는다(`prepare`가 `.git`이 없는 `web/`에서 돌아 husky가 설치되지 않는다).
- Biome가 끄는 규칙은 설정이 정본이다. 이 문서에 되풀이하지 않는다. 주요 강제 규칙: `noFloatingPromises`·`noMisusedPromises`(떠도는 Promise 금지), `useNullishCoalescing`(`??`), `noImportCycles`(순환 import 금지), `useSortedClasses`(Tailwind 클래스 정렬), `useFilenamingConvention`, `noParameterAssign`, `noUselessElse`, `noCommonJs`(ESM만).
- 커밋·PR 전에 `npm run check`를 스스로 돌린다. 타입은 `npx tsc --noEmit`으로 본다(빌드와 같은 strict 설정).

## 주석

- 주석은 한국어로 쓴다. "왜"를 적는다(`coding-style.md` "주석").
- 공개 유틸·훅·복잡한 컴포넌트에는 용도와 전제를 한 줄 적는다. 타입으로 드러나는 것은 되풀이하지 않는다.

## 타입

- `tsconfig`의 `strict`를 끄지 않는다. 특정 줄의 `@ts-ignore`/`@ts-expect-error`에는 이유 주석을 단다. `any`로 덮지 않는다.
- 외부 경계(fetch 응답, `JSON.parse`, 서드파티 반환)는 `unknown`으로 받고 바로 좁혀 구체 타입으로 바꾼다. 안쪽으로 `any`를 넘기지 않는다.
- 타입 추론으로 충분하면 타입을 적지 않는다. 공개 함수의 반환 타입은 계약이 분명하도록 적는다.
- 상태·종류는 문자열 리터럴 유니온이나 enum으로 나타낸다(`coding-style.md` "자원과 값 표현").

## React·Next

- App Router를 쓰지만 정적 export(`NEXT_EXPORT=1 next build`)로 만든 결과가 Go 바이너리에 들어간다. 런타임 서버가 없으므로 서버 컴포넌트의 데이터 조회, 서버 액션, 라우트 핸들러, 요청마다 하는 렌더링을 쓰지 않는다. 데이터는 클라이언트에서 `/api`로 가져온다. 그래서 `web/src`의 대부분이 `"use client"`다.
- 훅 규칙(조건부 호출 금지, 의존성 배열)을 지킨다. Biome가 본다.
- 비동기 호출의 Promise를 떠돌게 두지 않는다. `await`하거나 명시적으로 처리한다.
- API 호출은 `src/lib/api.ts`의 래퍼(`api`, `http`, SSE는 `sseUrl`)를 쓴다. `fetch`를 직접 쓸 때도 상대 경로 `/api/...`로 보낸다. dev에서 `next.config.mjs`가 백엔드(`:8787`)로 리라이트한다. 백엔드 주소를 컴포넌트에 하드코딩하지 않는다.
- 전역·공유 상태는 `src/stores/`의 기존 패턴을 따른다. 컴포넌트 밖 모듈 수준 가변 상태를 새로 만들지 않는다.

## 파일과 import

- import 경로는 별칭 `@/*`(= `src/*`)를 쓴다. 긴 상대 경로(`../../..`)를 쓰지 않는다.
- import 정렬·그룹은 Biome(`organizeImports`)가 한다. 손으로 맞추지 않는다.
- 순환 import를 만들지 않는다(`noImportCycles`가 막는다).
- **생성물은 건드리지 않는다**: `src/components/ui/`, `src/components/calendar/`는 생성 코드라 Biome 검사에서도 빠진다. 이 안을 직접 고치지 말고, 바꿔야 하면 생성 절차(`generate:presets` 등)나 래퍼 컴포넌트로 한다.

## 테스트

- 프런트엔드 테스트는 최소다. 있는 것은 `node --test`로 도는 `*.test.mjs`(순수 로직)다. 복잡한 순수 로직을 더할 때 같은 방식으로 테스트를 둔다. 테스트 규칙은 `testing.md` "프런트엔드"를 따른다.
