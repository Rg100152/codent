#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

if [ ! -f .env ]; then
  echo "Pehle ./install.sh chalayein (.env nahi mila)" >&2
  exit 1
fi

set -a
. ./.env
set +a

PORT="${PORT:-10000}"

if [ -f codeint-api.pid ] && kill -0 "$(cat codeint-api.pid)" 2>/dev/null; then
  echo "codeint-api already chal raha hai (PID $(cat codeint-api.pid))"
  exit 0
fi

nohup node server.mjs >> codeint-api.log 2>&1 &
echo $! > codeint-api.pid

for _ in $(seq 1 40); do
  if curl -fsS "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    echo "codeint-api shuru ho gaya: http://127.0.0.1:${PORT}/v1  (PID $(cat codeint-api.pid))"
    exit 0
  fi
  sleep 0.25
done

echo "Server start nahi hua. Log:" >&2
tail -40 codeint-api.log >&2
exit 1
