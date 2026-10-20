# syntax=docker/dockerfile:1
#
# 실행 이미지(이미지 안에서 컴파일하지 않는다): 자주 쓰는 도구만 설치하고 **미리 빌드한 Linux 단일 바이너리**를 넣는다.
# 바이너리는 CI의 binaries job이 교차 컴파일하고(순수 Go, QEMU 없음), 대상 아키텍처별로
# 빌드 컨텍스트의 dist/<TARGETARCH>/artex에 둔다. 그래서 멀티 아키텍처 빌드에서 arm64는 apt 층만 에뮬레이션하고
# Next·Go 컴파일은 에뮬레이션하지 않아 훨씬 빠르다.
#
# 로컬에서 이미지를 직접 빌드할 때는 바이너리를 먼저 만든다:
#   cd web && npm run build:static && cd ..
#   mkdir -p server/webui && cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   CGO_ENABLED=0 GOARCH=amd64 go build -o dist/amd64/artex-sandbox ./cmd/artex-sandbox
#   docker build -t artex:local .

# Node는 공식 이미지에서 복사한다. 공식 이미지는 Node 릴리스 키로 서명을 검증한 tarball로
# 만들어지고, 여기서는 태그와 digest를 함께 적어 빌드마다 같은 이미지를 쓴다.
# Debian 패키지(bookworm은 18)는 Playwright 요구(>=20)에 맞지 않는다.
FROM node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392 AS node

# 태그만 적으면 다시 가리킬 수 있으므로 공식 이미지의 멀티 아키텍처 인덱스 digest를 함께 적는다. 갱신은 Dependabot이 한다.
FROM python:3.12-slim-bookworm@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258
ARG TARGETARCH
# 자주 쓰는 도구: ripgrep / curl / vim과 정보 수집에 늘 쓰는 도구 몇 가지(필요에 따라 더하거나 뺀다).
# 서버는 root가 아닌 사용자로 돈다. nmap의 SYN 스캔(-sS)·OS 탐지는 raw 소켓이 필요하므로 nmap 실행 파일에만
# cap_net_raw를 준다. cap_net_admin은 주지 않는다. Docker 기본 capability 집합에 NET_ADMIN이 없어서 파일 cap에
# 들어 있으면 exec 자체가 실패한다. setcap을 담은 libcap2-bin(Debian 공식 저장소)은 빌드에서만 쓰고
# 서버 실행 파일·도우미에 cap을 준 뒤(아래) 지운다.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap libcap2-bin \
    && setcap cap_net_raw+eip /usr/bin/nmap \
    && rm -rf /var/lib/apt/lists/*
# nmap은 uid가 0이 아니면 raw 소켓 권한이 없다고 보고 SYN 스캔을 거부한다. 파일 cap이 있음을 알려 준다.
ENV NMAP_PRIVILEGED=1
# 두 이미지 모두 Debian bookworm(glibc)이라 node 바이너리를 그대로 옮길 수 있다.
# npm·npx는 실행 링크가 복사되지 않으므로 공식 이미지와 같은 상대 경로로 다시 만든다.
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
    && node --version && npm --version && npx --version
# Playwright MCP·CLI는 docker/playwright의 lockfile대로 npm ci로 설치한다.
# npm ci는 lockfile의 integrity 해시를 검사하므로 빌드마다 같은 패키지가 들어온다.
# playwright는 @playwright/mcp·cli가 정확히 의존하는 버전(alpha여도)에 맞춘다. 버전이 다르면
# playwright install이 받는 chromium 리비전이 MCP·CLI가 찾는 리비전과 달라진다.
# 설치한 패키지와 실행 파일을 전역 위치(/usr/local/lib/node_modules, /usr/local/bin)에
# 링크해 예전 `npm install -g`와 같은 자리에서 보이게 한다. 내장 browser MCP는
# `npx @playwright/mcp`로 뜨는데, npx는 PATH가 아니라 전역 node_modules와 전역 bin을 본다.
# 마지막 npx 실행은 동작 확인이면서, 그 패키지 메타데이터를 npm 캐시에 남겨
# 레지스트리에 닿지 않는 컨테이너에서도 npx가 캐시로 해석하게 한다(예전 이미지와 같은 동작).
# NODE_PATH: 어느 디렉터리에서든 require('playwright')가 된다.
# 이어서 chromium과 그 시스템 의존 패키지를 미리 넣어 컨테이너 첫 실행에 받지 않게 한다.
COPY docker/playwright/package.json docker/playwright/package-lock.json /opt/playwright/
ENV NODE_PATH=/opt/playwright/node_modules
# chromium을 root의 홈(~/.cache)이 아니라 모든 사용자가 읽는 고정 경로에 둔다. 서버는 artex 사용자로 돈다.
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
RUN cd /opt/playwright && npm ci --omit=dev \
    && mkdir -p /usr/local/lib/node_modules/@playwright \
    && for pkg in @playwright/mcp @playwright/cli playwright; do \
         ln -s "/opt/playwright/node_modules/$pkg" "/usr/local/lib/node_modules/$pkg"; \
       done \
    && for bin in playwright-mcp playwright-cli playwright; do \
         ln -s "/opt/playwright/node_modules/.bin/$bin" "/usr/local/bin/$bin"; \
       done \
    && npm cache clean --force \
    && cd / && npx --no-install @playwright/mcp --help >/dev/null \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
# 서버와 에이전트 명령을 돌릴 사용자. uid·gid는 호스트 볼륨 소유권을 맞출 때 쓰므로 고정한다(README "Docker 볼륨 소유권").
ARG ARTEX_UID=10001
# 에이전트 uid 분리(ARTEX_AGENT_UID/GID, 기본 꺼짐)를 켰을 때 에이전트 명령이 도는 사용자. 도우미의 허용 범위
# (20000-29999) 안에 둔다. 작업 공간은 ACL로 나누므로(#182) 서버(artex)가 이 그룹에 속할 필요는 없지만, #182 이전에
# 켜서 쓰던 작업 공간에 이 사용자가 남긴 파일을 서버가 그룹 권한으로 읽고 지울 수 있게 보조 그룹으로 둔다. 에이전트는
# 서버 그룹(artex)에 속하지 않아 도우미(0750 root:artex)를 실행하지 못한다(README "에이전트 uid 분리").
ARG ARTEX_AGENT_UID=20000
RUN groupadd --system --gid "$ARTEX_UID" artex \
    && groupadd --system --gid "$ARTEX_AGENT_UID" artex-agent \
    && useradd --system --uid "$ARTEX_UID" --gid artex --groups artex-agent --home-dir /home/artex --create-home --shell /usr/sbin/nologin artex \
    && useradd --system --uid "$ARTEX_AGENT_UID" --gid artex-agent --home-dir /home/artex-agent --create-home --shell /usr/sbin/nologin artex-agent
# 작업·대화형 세션마다 나눠 줄 에이전트 uid 풀(20001-21024, #166)의 passwd·group 항목. 셸은 nologin이고 홈은 없다.
# passwd 항목이 없으면 그 uid로 ssh가 "No user exists for uid"로 실패한다. 범위는 스크립트 안에 있다.
# --image는 계정 파일에 직접 덧붙이는 빠른 방식이다(호스트에서는 쓰지 않는다).
COPY --chmod=755 agent-pool-users.sh /tmp/agent-pool-users.sh
RUN /tmp/agent-pool-users.sh --image && rm /tmp/agent-pool-users.sh
WORKDIR /app
# /app 아래 실행 파일·스크립트·스킬은 root 소유에 읽기 전용(755/644)이다. 프롬프트 주입을 당한 에이전트가 같은 uid로
# 돌아도 서버 바이너리나 시작 스크립트를 바꿔 다음 재시작 때 자기 코드를 실행시키지 못하게 한다.
# 그래서 컨테이너에서는 원클릭 업데이트를 쓰지 않고 이미지 교체(docker compose pull && docker compose up -d)로만 올린다.
# 미리 빌드한 해당 아키텍처 바이너리(dist/amd64/artex 또는 dist/arm64/artex)
COPY --chmod=755 dist/${TARGETARCH}/artex /app/artex
# 에이전트 uid 분리 도우미. 서버 그룹만 실행하고(0750 root:artex), 파일 cap으로 uid를 바꾼다. 허용 uid 범위는 도우미 코드에
# 고정돼 있어 서버 uid가 이것으로 root가 될 수 없다. cap_kill은 기능을 켰을 때 엔트리포인트의 "artex-sandbox launch"가
# 서버에 ambient로 넘긴다(다른 uid가 된 에이전트 프로세스를 시간 초과·TaskStop에서 죽이는 데 쓴다). 서버 실행 파일에는
# 파일 cap을 두지 않는다. 두면 커널이 secure-exec로 실행해 Go 런타임이 패닉 추적을 지운다(기능을 꺼도 그렇다).
# 셸 대체 디렉터리(/app/artex-shell)는 기능을 켰을 때 서버의 PATH가 된다. 안의 bash는 도우미를 가리킨다. root 소유에
# 읽기 전용 이미지 층이라 서버도 에이전트도 바꾸거나 지울 수 없다.
COPY --chmod=750 dist/${TARGETARCH}/artex-sandbox /app/artex-sandbox
RUN chown root:artex /app/artex-sandbox \
    && setcap cap_kill,cap_setuid,cap_setgid+ep /app/artex-sandbox \
    && mkdir -m 0750 /app/artex-shell \
    && ln -s /app/artex-sandbox /app/artex-shell/bash \
    && chown -h root:artex /app/artex-shell /app/artex-shell/bash \
    && apt-get purge -y libcap2-bin \
    && rm -rf /var/lib/apt/lists/*
# 감시 시작 스크립트: 비정상 종료면 다시 띄우고, SIGTERM을 artex에 전달한다. docker stop은 PID 1에만 신호를 보내므로
# 전달하지 않으면 artex가 받지 못해 정상 종료를 하지 못하고 10초 뒤 SIGKILL로 강제 종료된다.
COPY --chmod=755 start.sh /app/start.sh
COPY skills/ /app/skills/
# 쓰기 경로는 작업 공간과 서버 키 두 곳뿐이다. 빈 볼륨이 처음 붙을 때 이미지의 소유권을 물려받도록 미리 만든다.
RUN mkdir -p /app/data /app/keys && chown artex:artex /app/data /app/keys && chmod 700 /app/keys
# 서버 키(jwt.key, oauth.key)는 작업 공간(/app/data) 밖의 /app/keys 에 둔다.
# 볼륨이어야 컨테이너를 다시 만들어도 키가 남아 웹 세션과 ChatGPT 구독 로그인이 유지된다.
ENV ARTEX_KEY_DIR=/app/keys
# data/(SQLite 등)와 keys/(서버 키)를 남기는 볼륨 지점
VOLUME ["/app/data", "/app/keys"]
EXPOSE 8787 8788
# 그룹을 적지 않는다. "artex:artex"로 적으면 Docker가 /etc/group의 보조 그룹(artex-agent)을 붙이지 않는다.
USER artex
# umask 002: 컨테이너가 ./skills(그룹 artex, setgid)에 만든 스킬 파일도 그룹 쓰기가 되게 해 호스트 사용자와 함께 고친다.
# 키 파일은 서버가 0600으로 만들므로 영향이 없다.
# 에이전트 uid 분리를 켰을 때(ARTEX_AGENT_UID/GID)만 도우미의 launch로 CAP_KILL을 ambient에 올려 start.sh를 실행한다.
# 끈 상태는 start.sh를 바로 실행한다(cap 없음).
ENTRYPOINT ["/bin/sh", "-c", "umask 002 && if [ -n \"${ARTEX_AGENT_UID}${ARTEX_AGENT_GID}\" ]; then exec /app/artex-sandbox launch -- /app/start.sh \"$@\"; fi; exec /app/start.sh \"$@\"", "start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
