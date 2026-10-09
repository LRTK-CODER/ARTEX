# syntax=docker/dockerfile:1
#
# 运行镜像（不在镜像里编译）：只装常用工具，放入**预编译好的 Linux 单二进制**。
# 二进制由 CI 的 binaries job 交叉编译（纯 Go、无 QEMU），按目标架构放在
# 构建上下文的 dist/<TARGETARCH>/artex。这样多架构构建时 arm64 只需模拟 apt 层，
# 不再模拟 Next/Go 编译，速度快得多。
#
# 로컬에서 이미지를 직접 빌드할 때는 바이너리를 먼저 만든다:
#   cd web && npm run build:static && cd ..
#   mkdir -p server/webui && cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .

# Node는 공식 이미지에서 복사한다. 공식 이미지는 Node 릴리스 키로 서명을 검증한 tarball로
# 만들어지고, 여기서는 태그와 digest를 함께 적어 빌드마다 같은 이미지를 쓴다.
# Debian 패키지(bookworm은 18)는 Playwright 요구(>=20)에 맞지 않는다.
FROM node:22-bookworm-slim@sha256:c3de60bf2f9dd0ac6370e6117950ff62d6e339527e7472301c9c78a017978392 AS node

FROM python:3.12-slim-bookworm
ARG TARGETARCH
# 常用工具：ripgrep / curl / vim，加一批 recon 常备件（按需增删）。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && rm -rf /var/lib/apt/lists/*
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
WORKDIR /app
# 预编译好的对应架构二进制（dist/amd64/artex 或 dist/arm64/artex）
COPY dist/${TARGETARCH}/artex /app/artex
# 守护启动脚本：进程退出后按退出码决定是否重新拉起，页面一键更新靠它完成换装。
# 它同时负责把 SIGTERM 转发给 artex —— docker stop 只把信号发给 PID 1，
# 不转发的话 artex 收不到、做不了优雅关闭，10 秒后被 SIGKILL 硬杀。
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# 서버 키(jwt.key, oauth.key)는 작업 공간(/app/data) 밖의 /app/keys 에 둔다.
# 볼륨이어야 컨테이너를 다시 만들어도 키가 남아 웹 세션과 ChatGPT 구독 로그인이 유지된다.
ENV ARTEX_KEY_DIR=/app/keys
# data/（SQLite 等）与 keys/（服务器密钥）持久化点
VOLUME ["/app/data", "/app/keys"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
