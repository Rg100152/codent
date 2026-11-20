#!/usr/bin/env node
/**
 * codeint-api — OpenAI-compatible gateway for local OpenCode models.
 *
 * Zero npm dependencies. Node.js 18+ required.
 *
 * Upstream: OpenCode V2 HTTP API (http://127.0.0.1:49374 by default).
 * Exposes:  /health, /v1/models, /v1/chat/completions,
 *           /v1/responses, /v1/messages
 */

import http from "node:http";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.dirname(fileURLToPath(import.meta.url));
const VERSION = "1.0.0";

/* ------------------------------------------------------------------ config */

function loadDotEnv(file) {
  if (!fs.existsSync(file)) return;
  for (const line of fs.readFileSync(file, "utf8").split(/\r?\n/)) {
    const m = line.match(/^\s*([A-Z][A-Z0-9_]*)\s*=\s*(.*?)\s*$/);
    if (!m || m[2].startsWith("#")) continue;
    let value = m[2];
    if (
      (value.startsWith('"') && value.endsWith('"')) ||
      (value.startsWith("'") && value.endsWith("'"))
    ) {
      value = value.slice(1, -1).replace(/\\(["\\$`])/g, "$1");
    }
    if (process.env[m[1]] === undefined) process.env[m[1]] = value;
  }
}

loadDotEnv(path.join(ROOT, ".env"));

const env = (key, fallback = "") => process.env[key] ?? fallback;
const envInt = (key, fallback) => {
  const n = Number.parseInt(env(key, String(fallback)), 10);
  return Number.isFinite(n) ? n : fallback;
};
const envBool = (key, fallback = false) => {
  const value = env(key, "").toLowerCase();
  if (!value) return fallback;
  return ["1", "true", "yes", "on"].includes(value);
};

const cfg = {
  host: env("HOST", "127.0.0.1"),
  port: envInt("PORT", 10000),
  apiKey: env("API_KEY", ""),
  baseUrl: env("OPENCODE_BASE_URL", "").replace(/\/+$/, ""),
  password: env("OPENCODE_PASSWORD", ""),
  username: env("OPENCODE_USERNAME", "opencode"),
  agent: env("OPENCODE_AGENT", "plan"),
  location: env("OPENCODE_LOCATION", ""),
  timeout: envInt("OPENCODE_TIMEOUT_MS", 600000),
  firstTokenTimeout: envInt("FIRST_TOKEN_TIMEOUT_MS", 20000),
  streamMode: env("STREAM_MODE", "text").toLowerCase(), // text | session
  keepSessions: envBool("OPENCODE_KEEP_SESSIONS", false),
  mock: envBool("MOCK_UPSTREAM", false),
  prefix: env("MODEL_PREFIX", "opencode/"),
  defaultModel: env("DEFAULT_MODEL", ""),
  modelTTL: envInt("MODEL_CACHE_MS", 300000),
};

const state = {
  baseUrl: cfg.baseUrl || "http://127.0.0.1:49374",
  password: cfg.password,
  models: [],
  defaultModel: null,
  modelsAt: 0,
};

/* ------------------------------------------------------------------ errors */

class AppError extends Error {
  constructor(status, message, code = "upstream_error") {
    super(message);
    this.status = status;
    this.code = code;
  }
}

function sanitize(text) {
  let out = String(text ?? "");
  if (state.password) out = out.split(state.password).join("***");
  return out;
}

/* ------------------------------------------------------------- discovery */

function passwordFromDisk() {
  const base =
    process.env.XDG_CONFIG_HOME || path.join(os.homedir(), ".config");
  const file = path.join(base, "opencode", "service.json");
  try {
    const parsed = JSON.parse(fs.readFileSync(file, "utf8"));
    return typeof parsed.password === "string" ? parsed.password : "";
  } catch {
    return "";
  }
}

async function discover() {
  state.password = cfg.password || passwordFromDisk();

  if (cfg.mock) {
    state.baseUrl = "http://mock.local";
    return;
  }

  if (cfg.baseUrl) {
    state.baseUrl = cfg.baseUrl;
    return;
  }

  // In V2 the managed service listens on 127.0.0.1:49374 in this environment.
  // Older/explicit servers commonly use 4096.
  const candidates = [
    "http://127.0.0.1:49374",
    "http://localhost:4096",
    "http://host.docker.internal:49374",
  ];

  for (const candidate of candidates) {
    try {
      const response = await fetch(`${candidate}/api/info`, {
        headers: upstreamHeaders(),
        signal: AbortSignal.timeout(1500),
      });
      if (response.ok) {
        state.baseUrl = candidate;
        return;
      }
    } catch {
      // try next candidate
    }
  }
  // Keep first candidate so diagnostics remain useful.
  state.baseUrl = candidates[0];
}

function upstreamHeaders(extra = {}) {
  const headers = { "content-type": "application/json", ...extra };
  if (state.password) {
    headers.authorization =
      "Basic " +
      Buffer.from(`${cfg.username}:${state.password}`).toString("base64");
  }
  return headers;
}

async function fetchUpstream(endpoint, options = {}) {
  if (cfg.mock) return mockResponse(endpoint, options);
  const signal = options.signal || AbortSignal.timeout(options.timeoutMs ?? cfg.timeout);
  return fetch(state.baseUrl + endpoint, {
    method: options.method || "GET",
    headers: upstreamHeaders(options.headers),
    body: options.body,
    signal,
  });
}

async function upstreamError(response) {
  let payload = null;
  let raw = "";
  try {
    raw = await response.text();
    payload = raw ? JSON.parse(raw) : null;
  } catch {
    // non-JSON upstream error
  }

  let message = sanitize(
    payload?.message ||
      payload?.error?.message ||
      raw ||
      `OpenCode upstream returned HTTP ${response.status}`,
  );

  // OpenCode sometimes wraps a provider error in a 502 while keeping the real
  // status (e.g. 429) inside the JSON body.
  const effectiveStatus = Number(payload?.status ?? payload?.error?.status ?? response.status);
  const reason = payload?.reason || "";
  let status = response.status >= 500 ? 502 : response.status;
  let code = "upstream_error";

  if (effectiveStatus === 429 || reason === "rate_limit") {
    code = "rate_limit_exceeded";
    status = 429;
    const retry = payload?.retryAfterMs ?? payload?.error?.retryAfterMs;
    if (retry) message += ` (retry after ${Math.ceil(retry / 1000)}s)`;
  } else if (effectiveStatus === 401 || effectiveStatus === 403 || response.status === 401 || response.status === 403) {
    if (!payload?.message) {
      code = "upstream_auth_error";
      status = 502;
      message =
        "OpenCode upstream authentication failed. Set OPENCODE_PASSWORD (or .env) " +
        "to the password from ~/.config/opencode/service.json";
    }
  } else if (effectiveStatus === 404 || response.status === 404) {
    code = "invalid_request_error";
    status = 400;
  }

  return new AppError(status, message, code);
}

/* ------------------------------------------------------------- mock mode */

const MOCK_MODELS = [
  {
    id: "mock-free-chat",
    providerID: "opencode",
    name: "Mock Free Chat",
    type: "chat",
    variants: [{ id: "low" }, { id: "high" }],
    time: { released: 1795180000000 },
    limit: { context: 128000, output: 8192 },
  },
  {
    id: "mock-coder",
    providerID: "opencode",
    name: "Mock Coder",
    type: "chat",
    variants: [],
    time: { released: 1795180000000 },
    limit: { context: 64000, output: 4096 },
  },
];

function jsonResponse(payload, status = 200) {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function mockResponse(endpoint, options = {}) {
  const method = options.method || "GET";
  if (endpoint === "/api/info" && method === "GET") {
    return jsonResponse({
      version: "mock",
      pid: 0,
      hostname: "mock",
      urls: [state.baseUrl],
      paths: {},
    });
  }
  if (endpoint.startsWith("/api/model") && method === "GET") {
    return jsonResponse({ location: { directory: ROOT }, data: MOCK_MODELS });
  }
  if (endpoint === "/api/experimental/generate/text" && method === "POST") {
    const body = JSON.parse(options.body || "{}");
    const prompt = String(body.prompt || "");
    if (prompt.includes("__RATE_LIMIT__")) {
      return jsonResponse(
        {
          _tag: "ProviderRequestError",
          reason: "rate_limit",
          status: 429,
          message: "mock free usage limit reached",
          retryAfterMs: 5000,
        },
        502,
      );
    }
    return jsonResponse({
      data: {
        text:
          `PONG from mock upstream.\n` +
          `Model: ${body.model?.id || "default"}\n` +
          `Prompt tail: ${prompt.slice(-100)}`,
      },
    });
  }
  if (endpoint === "/api/session" && method === "POST") {
    return jsonResponse({
      data: { id: `ses_mock_${crypto.randomUUID().slice(0, 8)}` },
    });
  }
  if (endpoint.startsWith("/api/session/") && endpoint.endsWith("/prompt") && method === "POST") {
    return jsonResponse({ data: { id: `msg_mock_${crypto.randomUUID().slice(0, 8)}` } });
  }
  if (endpoint.startsWith("/api/session/") && ["GET", "DELETE"].includes(method)) {
    return jsonResponse({ data: { id: "ses_mock" } });
  }
  if (endpoint.startsWith("/api/event")) {
    return new Response(
      ': heartbeat\n\ndata: {"id":"evt_mock","type":"server.connected","data":{}}\n\n',
      { status: 200, headers: { "content-type": "text/event-stream" } },
    );
  }
  return jsonResponse(
    { _tag: "NotFoundError", message: `mock: unknown endpoint ${endpoint}` },
    404,
  );
}

/* ----------------------------------------------------------------- models */

async function loadModels() {
  const response = await fetchUpstream("/api/model");
  if (!response.ok) throw await upstreamError(response);
  const payload = await response.json();
  const list = (payload.data || []).filter(
    (model) => model.type === "chat" && model.enabled !== false && model.status !== "deprecated",
  );

  state.models = list.map((model) => ({
    id: model.id,
    providerID: model.providerID,
    name: model.name || model.id,
    variants: (model.variants || []).map((variant) => variant.id),
    limit: model.limit || null,
    released: model.time?.released || null,
  }));

  state.defaultModel =
    state.models.find((model) => model.id === cfg.defaultModel) ||
    state.models[0] ||
    null;
  state.modelsAt = Date.now();
  return state.models;
}

async function models() {
  if (!state.models.length || Date.now() - state.modelsAt > cfg.modelTTL) {
    try {
      await loadModels();
    } catch (error) {
      if (!state.models.length) throw error;
      // keep serving stale models if refresh failed
    }
  }
  return state.models;
}

async function resolveModel(requested) {
  const available = await models();
  if (!requested) {
    if (!state.defaultModel) {
      throw new AppError(503, "No chat models are available from OpenCode", "model_unavailable");
    }
    return state.defaultModel;
  }

  let id = String(requested).trim();
  let variant = "";
  if (id.includes(":")) {
    const index = id.lastIndexOf(":");
    variant = id.slice(index + 1);
    id = id.slice(0, index);
  }
  if (cfg.prefix && id.startsWith(cfg.prefix)) id = id.slice(cfg.prefix.length);

  const found =
    available.find((model) => model.id === id) ||
    available.find((model) => `${model.providerID}/${model.id}` === id);

  if (!found) {
    throw new AppError(
      400,
      `Unknown model "${requested}". Call GET /v1/models for the available model IDs.`,
      "invalid_request_error",
    );
  }
  if (variant && found.variants.length && !found.variants.includes(variant)) {
    throw new AppError(
      400,
      `Unknown variant "${variant}" for model "${found.id}". Available: ${found.variants.join(", ") || "none"}`,
      "invalid_request_error",
    );
  }
  return { ...found, variant: variant || undefined };
}

function modelRef(model) {
  return {
    id: model.id,
    providerID: model.providerID,
    ...(model.variant ? { variant: model.variant } : {}),
  };
}

function modelName(model) {
  return `${cfg.prefix}${model.id}`;
}

/* ------------------------------------------------------------ text bridge */

function contentToText(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((part) => {
      if (typeof part === "string") return part;
      if (part?.type === "text" || part?.text) return String(part.text || "");
      if (part?.type === "input_text") return String(part.text || "");
      if (part?.type === "image_url" || part?.type === "image") return "[image omitted]";
      return "";
    })
    .filter(Boolean)
    .join("\n");
}

function capitalizeRole(role) {
  const value = String(role || "user");
  if (value === "assistant" || value === "system" || value === "user" || value === "tool") {
    return value[0].toUpperCase() + value.slice(1);
  }
  return "User";
}

function messagesToPrompt(messages, instructions = "") {
  const blocks = [];
  if (instructions) blocks.push(`System:\n${instructions}`);
  for (const message of Array.isArray(messages) ? messages : []) {
    const role = message?.role || "user";
    if (role === "system" || role === "developer") {
      blocks.push(`System:\n${contentToText(message.content)}`);
      continue;
    }
    const text = contentToText(message.content);
    if (!text) continue;
    blocks.push(`${capitalizeRole(role)}:\n${text}`);
  }
  if (!blocks.length) blocks.push("User:\n");
  return `${blocks.join("\n\n")}\n\nAssistant:\n`;
}

async function generateText(prompt, ref) {
  const response = await fetchUpstream("/api/experimental/generate/text", {
    method: "POST",
    body: JSON.stringify({ prompt, model: ref }),
  });
  if (!response.ok) throw await upstreamError(response);
  const payload = await response.json();
  return String(payload?.data?.text ?? "");
}

function splitText(text) {
  const parts = String(text).match(/\S+\s*|\s+/g) || [String(text)];
  const chunks = [];
  let current = "";
  for (const part of parts) {
    if (current && current.length + part.length > 48) {
      chunks.push(current);
      current = "";
    }
    current += part;
  }
  if (current) chunks.push(current);
  return chunks.length ? chunks : [String(text)];
}

/* ---------------------------------------------------------- HTTP helpers */

function sendJson(res, status, payload) {
  if (res.headersSent) return;
  const body = JSON.stringify(payload);
  res.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "content-length": Buffer.byteLength(body),
    "access-control-allow-origin": "*",
  });
  res.end(body);
}

function sendError(res, status, message, code = "upstream_error", type = "error") {
  sendJson(res, status, {
    error: { message: sanitize(message), type, param: null, code, status },
  });
}

function sseHeaders() {
  return {
    "content-type": "text/event-stream; charset=utf-8",
    "cache-control": "no-cache, no-transform",
    connection: "keep-alive",
    "x-accel-buffering": "no",
    "access-control-allow-origin": "*",
  };
}

function sseData(res, data) {
  res.write(`data: ${typeof data === "string" ? data : JSON.stringify(data)}\n\n`);
}

function sseEvent(res, event, data) {
  res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
}

function chunk(id, model, content, finishReason = null) {
  return {
    id,
    object: "chat.completion.chunk",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [
      {
        index: 0,
        delta: content ? { content } : {},
        finish_reason: finishReason,
      },
    ],
  };
}

function completion(id, model, text) {
  return {
    id,
    object: "chat.completion",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [
      {
        index: 0,
        message: { role: "assistant", content: text },
        finish_reason: "stop",
        logprobs: null,
      },
    ],
    usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
  };
}

function authorized(req) {
  if (!cfg.apiKey) return true;
  const header = req.headers.authorization || "";
  let supplied = "";
  if (header.toLowerCase().startsWith("bearer ")) supplied = header.slice(7).trim();
  else if (req.headers["x-api-key"]) supplied = String(req.headers["x-api-key"]);
  const a = Buffer.from(supplied);
  const b = Buffer.from(cfg.apiKey);
  return a.length === b.length && crypto.timingSafeEqual(a, b);
}

async function readBody(req) {
  const chunks = [];
  let total = 0;
  for await (const chunk of req) {
    total += chunk.length;
    if (total > 10 * 1024 * 1024) throw new AppError(413, "Request body too large", "invalid_request_error");
    chunks.push(chunk);
  }
  const raw = Buffer.concat(chunks).toString("utf8");
  if (!raw.trim()) return {};
  try {
    return JSON.parse(raw);
  } catch {
    throw new AppError(400, "Invalid JSON body", "invalid_request_error");
  }
}

/* -------------------------------------------------------- chat completions */

async function streamViaText(res, { id, model, prompt, ref }) {
  const text = await generateText(prompt, ref);
  for (const part of splitText(text)) sseData(res, chunk(id, model, part));
  sseData(res, chunk(id, model, "", "stop"));
  sseData(res, "[DONE]");
}

async function streamViaSession(res, { id, model, prompt, ref }) {
  const types = [
    "session.text.delta",
    "session.text.ended",
    "session.idle",
    "session.execution.succeeded",
    "session.execution.failed",
    "session.execution.interrupted",
  ];
  const query = types.map((type) => `types=${encodeURIComponent(type)}`).join("&");
  const controller = new AbortController();
  const seenOrdinals = new Set();
  let sessionID = "";
  let firstTokenSeen = false;
  let fallbackSent = false;
  let finished = false;
  let firstTokenTimer = null;
  let hardTimer = null;

  return await new Promise((resolve) => {
    const finish = (error) => {
      if (finished) return;
      finished = true;
      if (firstTokenTimer) clearTimeout(firstTokenTimer);
      if (hardTimer) clearTimeout(hardTimer);
      try {
        controller.abort();
      } catch {
        // already aborted
      }
      if (error) {
        sseData(res, {
          error: {
            message: sanitize(error.message || String(error)),
            type: "error",
            code: error.code || "upstream_error",
          },
        });
      } else {
        sseData(res, chunk(id, model, "", "stop"));
      }
      sseData(res, "[DONE]");
      res.end();
      if (sessionID && !cfg.keepSessions) {
        fetchUpstream(`/api/session/${sessionID}`, { method: "DELETE" }).catch(() => {});
      }
      resolve();
    };

    (async () => {
      const events = await fetchUpstream(`/api/event?${query}`, {
        headers: { accept: "text/event-stream" },
        signal: controller.signal,
      });
      if (!events.ok) throw await upstreamError(events);

      (async () => {
        const reader = events.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        while (!finished) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          let index;
          while ((index = buffer.indexOf("\n\n")) >= 0) {
            const frame = buffer.slice(0, index);
            buffer = buffer.slice(index + 2);
            const dataLine = frame.split("\n").find((line) => line.startsWith("data: "));
            if (!dataLine) continue;
            let event;
            try {
              event = JSON.parse(dataLine.slice(6));
            } catch {
              continue;
            }
            if (typeof event.data === "string") {
              try {
                event = { ...event, data: JSON.parse(event.data) };
              } catch {
                // ignore malformed nested payload
              }
            }
            const data = event.data || {};
            if (sessionID && data.sessionID && data.sessionID !== sessionID) continue;

            if (event.type === "session.text.delta" && data.delta && !fallbackSent) {
              firstTokenSeen = true;
              if (firstTokenTimer) clearTimeout(firstTokenTimer);
              if (data.ordinal !== undefined) seenOrdinals.add(data.ordinal);
              sseData(res, chunk(id, model, data.delta));
            } else if (event.type === "session.text.ended" && data.text && !fallbackSent) {
              firstTokenSeen = true;
              if (firstTokenTimer) clearTimeout(firstTokenTimer);
              if (data.ordinal === undefined || !seenOrdinals.has(data.ordinal)) {
                sseData(res, chunk(id, model, data.text));
              }
            } else if (
              event.type === "session.idle" ||
              event.type === "session.execution.succeeded"
            ) {
              finish();
            } else if (
              event.type === "session.execution.failed" ||
              event.type === "session.execution.interrupted"
            ) {
              finish(
                new AppError(
                  502,
                  data.error?.message || `OpenCode session ${event.type}`,
                  "upstream_error",
                ),
              );
            }
          }
        }
        finish();
      })().catch((error) => finish(error));

      const sessionResponse = await fetchUpstream("/api/session", {
        method: "POST",
        body: JSON.stringify({
          agent: cfg.agent,
          model: ref,
          title: "codeint-api",
          ...(cfg.location ? { location: { directory: cfg.location } } : {}),
        }),
      });
      if (!sessionResponse.ok) throw await upstreamError(sessionResponse);
      sessionID = (await sessionResponse.json()).data?.id || "";

      const promptResponse = await fetchUpstream(`/api/session/${sessionID}/prompt`, {
        method: "POST",
        body: JSON.stringify({ text: prompt }),
      });
      if (!promptResponse.ok) throw await upstreamError(promptResponse);

      firstTokenTimer = setTimeout(async () => {
        if (finished || firstTokenSeen) return;
        fallbackSent = true;
        try {
          const text = await generateText(prompt, ref);
          for (const part of splitText(text)) sseData(res, chunk(id, model, part));
        } catch {
          // finish below with a clear timeout error if fallback also fails
        }
        finish();
      }, cfg.firstTokenTimeout);

      hardTimer = setTimeout(
        () => finish(new AppError(504, "OpenCode streaming timed out", "upstream_timeout")),
        cfg.timeout,
      );
    })().catch((error) => finish(error));
  });
}

async function handleChat(req, res) {
  const body = await readBody(req);
  const model = await resolveModel(body.model);
  const name = modelName(model);
  const prompt = messagesToPrompt(body.messages, body.instructions || body.system);
  const id = `chatcmpl-${crypto.randomUUID()}`;

  if (body.stream === true) {
    res.writeHead(200, sseHeaders());
    if (cfg.streamMode === "session") {
      await streamViaSession(res, { id, model: name, prompt, ref: modelRef(model) });
    } else {
      try {
        await streamViaText(res, { id, model: name, prompt, ref: modelRef(model) });
        res.end();
      } catch (error) {
        sseData(res, {
          error: {
            message: sanitize(error.message || String(error)),
            type: "error",
            code: error.code || "upstream_error",
          },
        });
        sseData(res, "[DONE]");
        res.end();
      }
    }
    return;
  }

  const text = await generateText(prompt, modelRef(model));
  sendJson(res, 200, completion(id, name, text));
}

/* ---------------------------------------------------------- /v1/responses */

function responsesInputToMessages(input, instructions) {
  const messages = [];
  if (instructions) messages.push({ role: "system", content: instructions });
  if (typeof input === "string") {
    messages.push({ role: "user", content: input });
  } else if (Array.isArray(input)) {
    for (const item of input) {
      if (typeof item === "string") {
        messages.push({ role: "user", content: item });
        continue;
      }
      const role = item.role || "user";
      if (role === "system" || role === "developer") {
        messages.push({ role: "system", content: contentToText(item.content) });
      } else {
        messages.push({ role, content: contentToText(item.content ?? item.text) });
      }
    }
  }
  return messages;
}

function responseObject(id, model, text) {
  const messageID = `msg_${crypto.randomUUID().replace(/-/g, "").slice(0, 24)}`;
  return {
    id,
    object: "response",
    created_at: Math.floor(Date.now() / 1000),
    status: "completed",
    model,
    output: [
      {
        id: messageID,
        type: "message",
        status: "completed",
        role: "assistant",
        content: [{ type: "output_text", text, annotations: [] }],
      },
    ],
    parallel_tool_calls: true,
    tool_choice: "auto",
    tools: [],
    usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0 },
  };
}

async function handleResponses(req, res) {
  const body = await readBody(req);
  const model = await resolveModel(body.model);
  const name = modelName(model);
  const prompt = messagesToPrompt(
    responsesInputToMessages(body.input ?? body.prompt, body.instructions),
  );
  const id = `resp_${crypto.randomUUID().replace(/-/g, "").slice(0, 24)}`;

  if (body.stream === true) {
    res.writeHead(200, sseHeaders());
    try {
      const text = await generateText(prompt, modelRef(model));
      sseEvent(res, "response.created", {
        type: "response.created",
        response: { ...responseObject(id, name, ""), status: "in_progress" },
      });
      sseEvent(res, "response.output_item.added", {
        type: "response.output_item.added",
        output_index: 0,
        item: responseObject(id, name, "").output[0],
      });
      for (const part of splitText(text)) {
        sseEvent(res, "response.output_text.delta", {
          type: "response.output_text.delta",
          item_id: responseObject(id, name, "").output[0].id,
          output_index: 0,
          content_index: 0,
          delta: part,
        });
      }
      sseEvent(res, "response.completed", {
        type: "response.completed",
        response: responseObject(id, name, text),
      });
      res.end();
    } catch (error) {
      sseData(res, {
        error: {
          message: sanitize(error.message || String(error)),
          type: "error",
          code: error.code || "upstream_error",
        },
      });
      res.end();
    }
    return;
  }

  const text = await generateText(prompt, modelRef(model));
  sendJson(res, 200, responseObject(id, name, text));
}

/* ----------------------------------------------------------- /v1/messages */

function anthropicMessagesToPrompt(body) {
  const messages = [];
  if (body.system) {
    const system = Array.isArray(body.system)
      ? body.system.map((part) => contentToText(part.text ?? part.content ?? part)).join("\n")
      : contentToText(body.system);
    messages.push({ role: "system", content: system });
  }
  for (const message of body.messages || []) {
    messages.push({ role: message.role, content: contentToText(message.content) });
  }
  return messagesToPrompt(messages);
}

function anthropicObject(id, model, text) {
  return {
    id,
    type: "message",
    role: "assistant",
    model,
    content: [{ type: "text", text }],
    stop_reason: "end_turn",
    stop_sequence: null,
    usage: { input_tokens: 0, output_tokens: 0 },
  };
}

async function handleMessages(req, res) {
  const body = await readBody(req);
  const model = await resolveModel(body.model);
  const name = modelName(model);
  const prompt = anthropicMessagesToPrompt(body);
  const id = `msg_${crypto.randomUUID().replace(/-/g, "").slice(0, 24)}`;

  if (body.stream === true) {
    res.writeHead(200, sseHeaders());
    try {
      const text = await generateText(prompt, modelRef(model));
      sseEvent(res, "message_start", {
        type: "message_start",
        message: { ...anthropicObject(id, name, ""), usage: { input_tokens: 0, output_tokens: 1 } },
      });
      sseEvent(res, "content_block_start", {
        type: "content_block_start",
        index: 0,
        content_block: { type: "text", text: "" },
      });
      for (const part of splitText(text)) {
        sseEvent(res, "content_block_delta", {
          type: "content_block_delta",
          index: 0,
          delta: { type: "text_delta", text: part },
        });
      }
      sseEvent(res, "content_block_stop", { type: "content_block_stop", index: 0 });
      sseEvent(res, "message_delta", {
        type: "message_delta",
        delta: { stop_reason: "end_turn", stop_sequence: null },
        usage: { output_tokens: 1 },
      });
      sseEvent(res, "message_stop", { type: "message_stop" });
      res.end();
    } catch (error) {
      sseEvent(res, "error", {
        type: "error",
        error: {
          type: "error",
          message: sanitize(error.message || String(error)),
        },
      });
      res.end();
    }
    return;
  }

  const text = await generateText(prompt, modelRef(model));
  sendJson(res, 200, anthropicObject(id, name, text));
}

/* -------------------------------------------------------------- endpoints */

async function handleModels(res) {
  const available = await models();
  sendJson(res, 200, {
    object: "list",
    data: available.map((model) => ({
      id: modelName(model),
      object: "model",
      created: model.released ? Math.floor(model.released / 1000) : Math.floor(Date.now() / 1000),
      owned_by: model.providerID,
      ...(model.limit ? { limit: model.limit } : {}),
    })),
  });
}

async function handleHealth(res) {
  let upstreamStatus = "unknown";
  let healthy = true;
  try {
    const response = await fetchUpstream("/api/info", { timeoutMs: 3000 });
    upstreamStatus = response.ok ? "ok" : `http_${response.status}`;
    if (!response.ok) healthy = false;
  } catch (error) {
    upstreamStatus = sanitize(error.message || String(error));
    healthy = false;
  }
  sendJson(
    res,
    healthy ? 200 : 503,
    {
      status: healthy ? "ok" : "degraded",
      service: "codeint-api",
      version: VERSION,
      upstream: { url: state.baseUrl, status: upstreamStatus },
      models: state.models.length,
      mode: cfg.mock ? "mock" : cfg.streamMode,
      time: new Date().toISOString(),
    },
  );
}

function handleHome(req, res) {
  const host = req.headers.host || `127.0.0.1:${cfg.port}`;
  const base = `http://${host}/v1`;
  const html = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>codeint-api</title>
<style>body{font:15px/1.6 ui-monospace,monospace;max-width:820px;margin:40px auto;padding:0 20px;color:#ddd;background:#111}h1{color:#7ee787}pre{background:#1c1c1c;padding:14px;border-radius:8px;overflow:auto}a{color:#79c0ff}</style>
</head><body>
<h1>codeint-api v${VERSION}</h1>
<p>OpenAI-compatible gateway for local OpenCode models.</p>
<p>Base URL: <code>${base}</code> &nbsp;·&nbsp; <a href="/health">/health</a> &nbsp;·&nbsp; <a href="/v1/models">/v1/models</a></p>
<h3>Test</h3><pre>curl ${base}/models -H "Authorization: Bearer ${
    cfg.apiKey ? "<YOUR_API_KEY>" : "<no key required>"
  }"

curl ${base}/chat/completions \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer ${
    cfg.apiKey ? "<YOUR_API_KEY>" : "<no key required>"
  }" \\
  -d '{"model":"${cfg.prefix}${state.defaultModel?.id || "MODEL_ID"}",
       "messages":[{"role":"user","content":"Say PONG"}]}'</pre>
<h3>Cursor / Continue</h3><pre>Base URL: ${base}
API Key:  ${cfg.apiKey ? "<API_KEY from .env>" : "(none)"}
Model:    ${cfg.prefix}${state.defaultModel?.id || "MODEL_ID"}</pre>
</body></html>`;
  res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
  res.end(html);
}

const server = http.createServer(async (req, res) => {
  try {
    const url = new URL(req.url || "/", "http://localhost");
    const route = url.pathname.replace(/\/+$/, "") || "/";

    if (route === "/") return handleHome(req, res);
    if (route === "/health") return await handleHealth(res);

    if (!authorized(req)) {
      return sendError(
        res,
        401,
        "Invalid or missing API key. Send Authorization: Bearer <API_KEY>",
        "invalid_api_key",
        "invalid_request_error",
      );
    }

    if (route === "/v1/models" && req.method === "GET") return await handleModels(res);
    if (route === "/v1/chat/completions" && req.method === "POST") return await handleChat(req, res);
    if (route === "/v1/responses" && req.method === "POST") return await handleResponses(req, res);
    if (route === "/v1/messages" && req.method === "POST") return await handleMessages(req, res);
    if (route === "/v1") {
      return sendJson(res, 200, {
        service: "codeint-api",
        version: VERSION,
        endpoints: ["/v1/models", "/v1/chat/completions", "/v1/responses", "/v1/messages"],
      });
    }

    return sendError(res, 404, `Unknown endpoint: ${route}`, "invalid_request_error");
  } catch (error) {
    const status = error instanceof AppError ? error.status : 500;
    const code = error instanceof AppError ? error.code : "internal_error";
    console.error(`[error] ${req.method} ${req.url}: ${sanitize(error.stack || error.message)}`);
    if (res.headersSent) {
      try {
        sseData(res, "[DONE]");
        res.end();
      } catch {
        // client already gone
      }
      return;
    }
    sendError(res, status, error.message || "Internal error", code);
  }
});

server.on("clientError", (error, socket) => {
  socket.end("HTTP/1.1 400 Bad Request\r\n\r\n");
});

await discover();

try {
  await loadModels();
  console.log(`[codeint] upstream: ${state.baseUrl} — ${state.models.length} chat models`);
} catch (error) {
  console.warn(`[codeint] warning: could not load models: ${sanitize(error.message)}`);
}

if (!cfg.apiKey) {
  console.warn("[codeint] warning: API_KEY is empty — anyone who can reach this port can use the API.");
}

server.listen(cfg.port, cfg.host, () => {
  console.log(`[codeint] ready at http://${cfg.host}:${cfg.port}/v1  (mode: ${cfg.mock ? "mock" : cfg.streamMode})`);
  if (state.defaultModel) console.log(`[codeint] default model: ${modelName(state.defaultModel)}`);
});

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => server.close(() => process.exit(0)));
}
