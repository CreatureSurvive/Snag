// Captures the dashboard screenshots in docs/screenshots with headless Chrome, signed in to the
// server described by demo.env. Usage: node Scripts/screenshots/dashboard.mjs demo.env
import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync, existsSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const env = Object.fromEntries(readFileSync(process.argv[2], "utf8").split("\n").filter((l) => l.includes("=")).map((l) => [l.slice(0, l.indexOf("=")), l.slice(l.indexOf("=") + 1).trim()]));
const base = env.SNAG_PUBLIC_URL;
const out = join(dirname(fileURLToPath(import.meta.url)), "../../docs/screenshots");
const chrome = process.env.CHROME ?? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";

// Sign in through the login form.
const login = await fetch(base + "/login", {
  method: "POST", redirect: "manual",
  headers: { "Content-Type": "application/x-www-form-urlencoded" },
  body: new URLSearchParams({ user: "admin", password: env.SNAG_ADMIN_PASSWORD, next: "/" }),
});
const cookie = login.headers.getSetCookie().map((c) => c.split(";")[0]).find((c) => c.includes("="));
if (!cookie) throw new Error(`sign-in failed: ${login.status}`);
const [cookieName, cookieValue] = [cookie.slice(0, cookie.indexOf("=")), cookie.slice(cookie.indexOf("=") + 1)];

const profile = mkdtempSync(join(tmpdir(), "snag-shots-"));
const proc = spawn(chrome, ["--headless=new", "--remote-debugging-port=0", `--user-data-dir=${profile}`, "--hide-scrollbars", "--no-first-run", "about:blank"], { stdio: "ignore" });
const portFile = join(profile, "DevToolsActivePort");
for (let i = 0; i < 100 && !existsSync(portFile); i++) await new Promise((r) => setTimeout(r, 100));
const [port, path] = readFileSync(portFile, "utf8").trim().split("\n");

const ws = new WebSocket(`ws://127.0.0.1:${port}${path}`);
await new Promise((r) => ws.addEventListener("open", r, { once: true }));
let nextID = 0;
const pending = new Map(), waiters = [];
ws.addEventListener("message", (e) => {
  const msg = JSON.parse(e.data);
  if (msg.id && pending.has(msg.id)) {
    const { resolve, reject } = pending.get(msg.id);
    pending.delete(msg.id);
    msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result);
  } else if (msg.method) {
    for (const w of [...waiters]) if (w.method === msg.method) { waiters.splice(waiters.indexOf(w), 1); w.resolve(msg); }
  }
});
const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
  const id = ++nextID;
  pending.set(id, { resolve, reject });
  ws.send(JSON.stringify({ id, method, params, sessionId }));
});

const { targetId } = await send("Target.createTarget", { url: "about:blank" });
const { sessionId } = await send("Target.attachToTarget", { targetId, flatten: true });
const page = (method, params) => send(method, params, sessionId);
const event = (method) => new Promise((resolve) => waiters.push({ method, resolve }));
await page("Page.enable");
await page("Network.enable");
await page("Network.setCookie", { name: cookieName, value: cookieValue, url: base });

async function shoot(name, url, { width = 1280, height = 820, full = false, maxHeight = 1800, scheme = "light" } = {}) {
  await page("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 2, mobile: false });
  await page("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: scheme }] });
  const loaded = event("Page.loadEventFired");
  await page("Page.navigate", { url: base + url });
  await loaded;
  await new Promise((r) => setTimeout(r, 600));
  let clip;
  if (full) {
    const { result } = await page("Runtime.evaluate", { expression: "document.documentElement.scrollHeight" });
    clip = { x: 0, y: 0, width, height: Math.min(result.value, maxHeight), scale: 1 };
  }
  const { data } = await page("Page.captureScreenshot", { format: "png", captureBeyondViewport: full, clip });
  writeFileSync(join(out, `${name}.png`), Buffer.from(data, "base64"));
  console.log("saved", name);
}

const issue = process.argv[3] ?? "1";
await shoot("dashboard-issues", "/p/reelhouse");
await shoot("dashboard-issue", `/p/reelhouse/issues/${issue}`, { full: true });
await shoot("dashboard-board", "/b/reelhouse", { width: 1000, height: 700, full: true });
await shoot("dashboard-issues-dark", "/p/reelhouse", { scheme: "dark" });

ws.close();
const exited = new Promise((r) => proc.once("exit", r));
proc.kill();
await exited;
rmSync(profile, { recursive: true, force: true, maxRetries: 5 });
