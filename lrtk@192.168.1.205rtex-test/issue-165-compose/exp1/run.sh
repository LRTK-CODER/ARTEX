#!/bin/sh
export PATH=/opt/shim:$PATH ARTEX_AGENT_UID=20000 ARTEX_AGENT_GID=20000
echo "server: $(id)"; grep CapEff /proc/self/status
/app/artex-sandbox check
bash -c 'echo "agent: $(id)"; umask; echo PATH=$PATH; grep -E "CapEff|NoNewPrivs" /proc/self/status; cat /keys/oauth.key; cat /proc/1/environ >/dev/null; echo x > /work/a && ls -l /work/a; bash -c "echo nested \$(id -u)"; /app/artex-sandbox check; nmap -sS -p 22 127.0.0.1 2>&1 | tail -3'
echo "== exec mode"; /app/artex-sandbox exec -- id
echo "== bad uid"; ARTEX_AGENT_UID=0 bash -c id
