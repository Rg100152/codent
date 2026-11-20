#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

if [ ! -f codeint-api.pid ]; then
  echo "Koi PID file nahi mila — server chal hi nahi raha hoga."
  exit 0
fi

PID="$(cat codeint-api.pid)"
if kill -0 "$PID" 2>/dev/null; then
  kill "$PID"
  echo "codeint-api band kiya gaya (PID $PID)"
else
  echo "PID $PID ab zinda nahi hai."
fi
rm -f codeint-api.pid
