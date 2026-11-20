# codeint-api

Local **OpenCode** ke free models ko ek **OpenAI-compatible API** mein badalne wala chhota gateway.
Zero npm dependencies — sirf `node server.mjs`.

```
Cursor / Continue / Cline / OpenAI SDK
              │  /v1/chat/completions
              ▼
       codeint-api (port 10000)
              │  OpenCode V2 HTTP API (Basic auth)
              ▼
   opencode service → free models
```

## Quick start (ek command)

```bash
./install.sh
```

Ye script:
1. Node 18+ check karta hai
2. `.env` banata hai (na ho to) aur random `API_KEY` generate karta hai
3. `~/.config/opencode/service.json` se OpenCode service password detect karta hai
4. `opencode api get /api/info` se service URL detect karta hai
5. **Self-test** chalata hai (mock upstream — free quota use nahi hota)
6. Server background mein start karta hai

Baad mein control:

```bash
./stop.sh      # band karein
./start.sh     # dobara shuru karein
node test.mjs  # sirf self-test
```

## Settings

`curl` se dekhein:

```bash
curl http://127.0.0.1:10000/health
```

| Key | Default | Matlab |
|---|---|---|
| `HOST` / `PORT` | `127.0.0.1` / `10000` | Gateway bind address |
| `API_KEY` | random | Clients ko `Authorization: Bearer <key>` bhejna padega |
| `OPENCODE_BASE_URL` | auto | OpenCode service URL (auto: `127.0.0.1:49374`, phir `4096`) |
| `OPENCODE_PASSWORD` | auto | `~/.config/opencode/service.json` se padha jata hai |
| `OPENCODE_AGENT` | `plan` | Chat ke liye read-only agent (`build` se tool side effects mil sakte hain) |
| `STREAM_MODE` | `text` | `text` = stable SSE chunks; `session` = experimental live event stream |
| `MODEL_PREFIX` | `opencode/` | Model ID prefix, jaise `opencode/tidepool-alpha-free` |
| `DEFAULT_MODEL` | pehla model | Model jab request mein `model` na ho |
| `MOCK_UPSTREAM` | `0` | `1` = fake upstream (testing ke liye) |

## Endpoints

| Endpoint | Support |
|---|---|
| `GET /health` | Gateway + upstream status |
| `GET /v1/models` | Free chat models (`opencode/<model-id>`) |
| `POST /v1/chat/completions` | Non-stream + SSE stream (`stream: true`) |
| `POST /v1/responses` | OpenAI Responses API (basic) |
| `POST /v1/messages` | Anthropic Messages API (basic, stream events bhi) |

### Examples

```bash
KEY=$(grep '^API_KEY=' .env | cut -d= -f2)

# Models
curl http://127.0.0.1:10000/v1/models -H "Authorization: Bearer $KEY"

# Chat
curl http://127.0.0.1:10000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $KEY" \
  -d '{"model":"opencode/mock-free-chat","messages":[{"role":"user","content":"Say PONG"}]}'

# Streaming
curl -N http://127.0.0.1:10000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $KEY" \
  -d '{"model":"opencode/mock-free-chat","stream":true,"messages":[{"role":"user","content":"Hi"}]}'
```

### Cursor / Continue / Cline

```
Base URL : http://127.0.0.1:10000/v1
API Key  : <API_KEY from .env>
Model    : opencode/<model-id>     # GET /v1/models se list dekhein
```

Variant chahiye to `opencode/tidepool-alpha-free:high` jaisa model ID use karein (sirf tab jab model us variant ko support kare).

## Docker (optional)

```bash
cp .env.example .env   # API_KEY aur OPENCODE_PASSWORD bharein
docker compose up -d --build
curl http://127.0.0.1:10000/health
```

⚠️ OpenCode V2 service **sirf 127.0.0.1** par listen karti hai. Isliye `docker-compose.yml` mein `network_mode: host` hai — ye **Linux** par kaam karta hai. macOS/Windows par Docker Desktop host networking support nahi karta, wahan `./install.sh` (local Node) istemal karein.

## Streaming modes

- `STREAM_MODE=text` (default): upstream `/api/experimental/generate/text` se poora text lekar SSE chunks mein bhejta hai. Stable aur quota-friendly.
- `STREAM_MODE=session`: OpenCode session banakar `/api/event` se `session.text.delta` events stream karta hai. Real-time hai, lekin experimental; pehle 20s (`FIRST_TOKEN_TIMEOUT_MS`) mein token na aaye to `generate/text` se fallback karta hai. Session ko `plan` agent ke saath banata hai taaki tool side effects na hon; `OPENCODE_KEEP_SESSIONS=1` se sessions delete hone se bachein.

## Free quota note

Inference OpenCode ke free tier se aati hai. Jab quota khatam ho jata hai to upstream **`429` + `rate_limit`** bhejta hai aur gateway wahi error OpenAI-style format mein passthrough karta hai:

```json
{"error":{"message":"Free usage limit reached ... (retry after 29835s)","code":"rate_limit_exceeded","status":429}}
```

Ye gateway paid keys ko bhi support kar sakta hai agar aap OpenCode config mein apna provider add karein — gateway ko sirf model list badalni padegi.

## Troubleshooting

- **401 upstream auth error** → `OPENCODE_PASSWORD` galat/missing. `~/.config/opencode/service.json` dekhein.
- **No models / 503** → `opencode service status` aur `opencode api get /api/info` chalayein.
- **Port already in use** → `.env` mein `PORT` badlein, `./stop.sh` se purana instance band karein.
- **429** → free quota reset hone ka intezaar karein ya apna provider configure karein.

## Files

```
server.mjs         Gateway (no dependencies)
setup-env.mjs      .env setup + key generation
install.sh         One-command install
start.sh/stop.sh   Background service control
test.mjs           Mock-upstream self-test
docker-compose.yml Linux Docker deployment
```
