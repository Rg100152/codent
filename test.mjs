#!/usr/bin/env node
/** End-to-end self-test using a fake upstream (no OpenCode quota required). */

import { spawn } from "node:child_process";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.dirname(fileURLToPath(import.meta.url));
const PORT = process.env.TEST_PORT || "10001";
const API_KEY = "test-key-1234567890abcdef";
const base = `http://127.0.0.1:${PORT}`;

const child = spawn(process.execPath, ["server.mjs"], {
  cwd: ROOT,
  env: {
    ...process.env,
    MOCK_UPSTREAM: "1",
    HOST: "127.0.0.1",
    PORT,
    API_KEY,
    OPENCODE_BASE_URL: "",
    OPENCODE_PASSWORD: "",
    MODEL_PREFIX: "opencode/",
    STREAM_MODE: "text",
  },
  stdio: ["ignore", "pipe", "pipe"],
});

let logs = "";
child.stdout.on("data", (chunk) => (logs += chunk));
child.stderr.on("data", (chunk) => (logs += chunk));

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function waitReady() {
  for (let i = 0; i < 60; i += 1) {
    try {
      const response = await fetch(`${base}/health`);
      if (response.ok) return;
    } catch {
      // not listening yet
    }
    await sleep(100);
  }
  throw new Error(`Server ready nahi hua:\n${logs}`);
}

async function test(name, fn) {
  await fn();
  console.log(`  PASS  ${name}`);
}

try {
  await waitReady();
  const headers = {
    authorization: `Bearer ${API_KEY}`,
    "content-type": "application/json",
  };

  await test("health", async () => {
    const payload = await (await fetch(`${base}/health`)).json();
    assert.equal(payload.status, "ok");
    assert.equal(payload.mode, "mock");
  });

  await test("GET /v1/models", async () => {
    const response = await fetch(`${base}/v1/models`, { headers });
    assert.equal(response.status, 200);
    const payload = await response.json();
    assert.equal(payload.object, "list");
    assert.ok(payload.data.length >= 1);
    assert.equal(payload.data[0].id, "opencode/mock-free-chat");
  });

  await test("auth required", async () => {
    const response = await fetch(`${base}/v1/models`);
    assert.equal(response.status, 401);
  });

  await test("POST /v1/chat/completions (non-stream)", async () => {
    const response = await fetch(`${base}/v1/chat/completions`, {
      method: "POST",
      headers,
      body: JSON.stringify({
        model: "opencode/mock-free-chat",
        messages: [{ role: "user", content: "Say PONG" }],
      }),
    });
    assert.equal(response.status, 200);
    const payload = await response.json();
    assert.equal(payload.object, "chat.completion");
    assert.match(payload.choices[0].message.content, /PONG from mock upstream/);
  });

  await test("POST /v1/chat/completions (SSE stream)", async () => {
    const response = await fetch(`${base}/v1/chat/completions`, {
      method: "POST",
      headers,
      body: JSON.stringify({
        model: "opencode/mock-free-chat",
        stream: true,
        messages: [{ role: "user", content: "Say PONG" }],
      }),
    });
    assert.equal(response.status, 200);
    assert.match(response.headers.get("content-type") || "", /text\/event-stream/);
    const text = await response.text();
    assert.match(text, /data: \{"id":"chatcmpl-/);
    assert.match(text, /"finish_reason":"stop"/);
    assert.match(text, /data: \[DONE\]/);
  });

  await test("POST /v1/responses", async () => {
    const response = await fetch(`${base}/v1/responses`, {
      method: "POST",
      headers,
      body: JSON.stringify({ model: "mock-free-chat", input: "Say PONG" }),
    });
    assert.equal(response.status, 200);
    const payload = await response.json();
    assert.equal(payload.object, "response");
    assert.equal(payload.output[0].type, "message");
    assert.match(payload.output[0].content[0].text, /PONG/);
  });

  await test("POST /v1/messages", async () => {
    const response = await fetch(`${base}/v1/messages`, {
      method: "POST",
      headers: { ...headers, "x-api-key": API_KEY },
      body: JSON.stringify({
        model: "mock-free-chat",
        max_tokens: 64,
        messages: [{ role: "user", content: "Say PONG" }],
      }),
    });
    assert.equal(response.status, 200);
    const payload = await response.json();
    assert.equal(payload.type, "message");
    assert.equal(payload.content[0].type, "text");
  });

  await test("Anthropic stream events", async () => {
    const response = await fetch(`${base}/v1/messages`, {
      method: "POST",
      headers,
      body: JSON.stringify({
        model: "mock-free-chat",
        stream: true,
        messages: [{ role: "user", content: "Say PONG" }],
      }),
    });
    const text = await response.text();
    assert.match(text, /event: message_start/);
    assert.match(text, /event: content_block_delta/);
    assert.match(text, /event: message_stop/);
  });

  await test("wrapped 429 is mapped to 429", async () => {
    const response = await fetch(`${base}/v1/chat/completions`, {
      method: "POST",
      headers,
      body: JSON.stringify({
        model: "mock-free-chat",
        messages: [{ role: "user", content: "__RATE_LIMIT__" }],
      }),
    });
    assert.equal(response.status, 429);
    const payload = await response.json();
    assert.equal(payload.error.code, "rate_limit_exceeded");
  });

  await test("unknown model -> 400", async () => {
    const response = await fetch(`${base}/v1/chat/completions`, {
      method: "POST",
      headers,
      body: JSON.stringify({ model: "not-a-real-model", messages: [] }),
    });
    assert.equal(response.status, 400);
    const payload = await response.json();
    assert.equal(payload.error.code, "invalid_request_error");
  });

  console.log("\nALL TESTS PASSED");
} catch (error) {
  console.error("\nTEST FAILED:", error.message);
  if (logs) console.error(`\n--- server log ---\n${logs}`);
  process.exitCode = 1;
} finally {
  child.kill("SIGTERM");
}
