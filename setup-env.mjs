#!/usr/bin/env node
/** Creates/updates .env with a generated API key and detected OpenCode settings. */

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import crypto from "node:crypto";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const exec = promisify(execFile);
const ROOT = path.dirname(fileURLToPath(import.meta.url));
const envPath = path.join(ROOT, ".env");

function parseEnv(text) {
  const out = {};
  for (const line of text.split(/\r?\n/)) {
    const m = line.match(/^\s*([A-Z][A-Z0-9_]*)\s*=\s*(.*?)\s*$/);
    if (!m) continue;
    let value = m[2];
    if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
      value = value.slice(1, -1).replace(/\\(["\\$`])/g, "$1");
    }
    out[m[1]] = value;
  }
  return out;
}

function setKey(values, key, value) {
  const lines = fs.readFileSync(envPath, "utf8").split(/\r?\n/);
  // Quote values that would break `source .env` in start.sh.
  const serialized = /^[A-Za-z0-9_./:@-]+$/.test(value)
    ? value
    : `"${value.replace(/\\/g, "\\\\").replace(/"/g, '\\"').replace(/\$/g, "\\$").replace(/`/g, "\\`")}"`;
  let found = false;
  const updated = lines.map((line) => {
    if (new RegExp(`^\\s*${key}\\s*=`).test(line)) {
      found = true;
      return `${key}=${serialized}`;
    }
    return line;
  });
  if (!found) updated.push(`${key}=${serialized}`);
  fs.writeFileSync(envPath, updated.join("\n") + "\n", { mode: 0o600 });
  values[key] = value;
}

if (!fs.existsSync(envPath)) {
  fs.copyFileSync(path.join(ROOT, ".env.example"), envPath, fs.constants.COPYFILE_EXCL);
  fs.chmodSync(envPath, 0o600);
  console.log("[setup] .env banaya gaya");
}

const values = parseEnv(fs.readFileSync(envPath, "utf8"));

if (!values.API_KEY || values.API_KEY.startsWith("change-me") || values.API_KEY.length < 16) {
  const key = crypto.randomBytes(24).toString("hex");
  setKey(values, "API_KEY", key);
  console.log("[setup] naya API_KEY generate kiya gaya");
} else {
  console.log("[setup] maujooda API_KEY use kiya ja raha hai");
}

if (!values.OPENCODE_PASSWORD) {
  const base = process.env.XDG_CONFIG_HOME || path.join(os.homedir(), ".config");
  const file = path.join(base, "opencode", "service.json");
  try {
    const password = JSON.parse(fs.readFileSync(file, "utf8")).password;
    if (password) {
      setKey(values, "OPENCODE_PASSWORD", password);
      console.log("[setup] OpenCode service password service.json se mil gaya");
    }
  } catch {
    console.log("[setup] service.json nahi mila — Docker mode mein OPENCODE_PASSWORD manually set karein");
  }
}

if (!values.OPENCODE_BASE_URL) {
  try {
    const { stdout } = await exec("opencode", ["api", "get", "/api/info"], {
      timeout: 5000,
      env: process.env,
    });
    const info = JSON.parse(stdout);
    const url = Array.isArray(info.urls) ? info.urls[0] : null;
    if (url) {
      setKey(values, "OPENCODE_BASE_URL", url);
      console.log(`[setup] OpenCode service mil gaya: ${url}`);
    }
  } catch {
    console.log("[setup] opencode CLI se service detect nahi hua — default 127.0.0.1:49374 use hoga");
  }
}

const port = values.PORT || "10000";
console.log("");
console.log("===== SETUP COMPLETE =====");
console.log(`Base URL : http://127.0.0.1:${port}/v1`);
console.log(`API Key  : ${values.API_KEY}`);
console.log("");
console.log("Test karne ke liye:");
console.log(`curl http://127.0.0.1:${port}/health`);
console.log(`curl http://127.0.0.1:${port}/v1/models -H "Authorization: Bearer ${values.API_KEY}"`);
