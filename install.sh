#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

echo "== codeint-api installer =="

if ! command -v node >/dev/null 2>&1; then
  echo "Node.js nahi mila. Node 18+ install karein: https://nodejs.org/" >&2
  exit 1
fi

NODE_MAJOR="$(node -p 'process.versions.node.split(".")[0]')"
if [ "$NODE_MAJOR" -lt 18 ]; then
  echo "Node 18+ zaroori hai (mila: $(node --version))." >&2
  exit 1
fi

echo "Node $(node --version) OK"

node setup-env.mjs

echo ""
echo "Self-test (mock upstream) chal raha hai..."
node test.mjs

if [ "${NO_START:-0}" != "1" ]; then
  echo ""
  ./start.sh
fi

echo ""
echo "Sab kuch ready hai. Cursor/Continue mein:"
echo "  Base URL: http://127.0.0.1:$(grep -E '^PORT=' .env | cut -d= -f2 || echo 10000)/v1"
echo "  API Key : $(grep -E '^API_KEY=' .env | cut -d= -f2)"
echo "  Model   : $(grep -E '^MODEL_PREFIX=' .env | cut -d= -f2 || echo opencode/)<model-id>  (GET /v1/models dekhein)"
