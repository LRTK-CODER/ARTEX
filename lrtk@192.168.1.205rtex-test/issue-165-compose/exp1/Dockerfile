FROM debian:bookworm-slim
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends libcap2-bin nmap procps >/dev/null && \
    groupadd -g 10001 artex && groupadd -g 20000 artex-agent && \
    useradd -u 10001 -g 10001 -G artex-agent -m artex && useradd -u 20000 -g 20000 -M -s /usr/sbin/nologin artex-agent && \
    setcap cap_net_raw+eip /usr/bin/nmap && mkdir -p /keys /work && echo SECRET > /keys/oauth.key && \
    chown 10001:10001 /keys /keys/oauth.key && chmod 700 /keys && chmod 600 /keys/oauth.key && \
    chown 10001:20000 /work && chmod 2770 /work
COPY artex-sandbox /app/artex-sandbox
RUN chown root:10001 /app/artex-sandbox && chmod 750 /app/artex-sandbox && setcap cap_setuid,cap_setgid+ep /app/artex-sandbox && \
    mkdir /opt/shim && ln -s /app/artex-sandbox /opt/shim/bash && chown -h root:10001 /opt/shim/bash
ENV NMAP_PRIVILEGED=1
USER artex:artex
