// Pairing and provider selection through the real settings page, with a
// local API fixture. No peer, vendor, credential store or live app is used.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { strings } = require("./strings.cjs");
const assets = path.resolve(__dirname, "../assets");
const dict = strings().I18N;

function fixture(lang) {
  const settings = { lang, theme: "light", dir: "/tmp/peer-test", version: "test", gateway: "http://127.0.0.1:3425", fx: { rate: 7.2 }, visionModels: [], imageGenModels: [] };
  const state = { peers: [], writes: [], syncError: "", readError: "", statusError: "", saveError: "" };
  const token = "test-only-pairing-token-" + "x".repeat(40);
  const providers = [
    { id: "api", name: "Local API", models: ["model-one", "model-two"], canSync: true },
    { id: "claude", name: "Claude", models: ["claude-model"], canSync: false },
  ];
  state.serve = async (route) => {
    const req = route.request(), pathname = new URL(req.url()).pathname;
    const json = (data, status = 200) => route.fulfill({ json: data, status });
    const error = (error) => json({ error }, 400);
    if (pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:${JSON.stringify(lang)},theme:"light",web:false};` });
    if (pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (pathname === "/api/state") return json({ agents: [], profiles: [], settings });
    if (pathname === "/api/settings") return json(settings);
    if (pathname === "/api/usage/quotas") return json([]);
    if (pathname === "/api/plugins") return json({ plugins: [] });
    if (pathname === "/api/groups") return json({ groups: [], models: [] });
    if (pathname === "/api/davsync") return json({ on: false });
    if (pathname === "/api/peers") return state.readError ? error(state.readError) : json({ peers: state.peers.map(({ token, ...p }) => p), providers });
    if (pathname === "/api/peers/status") return state.statusError ? error(state.statusError) : json(state.peers.map((p) => ({ id: p.id, connected: true, status: "connected" })));
    if (pathname === "/api/peers/draft") return json({ version: 1, id: "office", token });
    if (pathname.startsWith("/api/peers/")) {
      const body = req.postDataJSON();
      state.writes.push({ pathname, body });
      if (pathname.endsWith("/save")) {
        if (state.saveError) return error(state.saveError);
        const idx = state.peers.findIndex((p) => p.id === body.peer.id);
        const next = { ...body.peer, token: body.peer.token || state.peers[idx]?.token, revision: `revision-${state.writes.length}` };
        if (idx < 0) state.peers.push(next); else state.peers[idx] = next;
        return json({ ok: true });
      }
      if (pathname.endsWith("/pairing")) {
        const p = state.peers.find((p) => p.id === body.id);
        return json({ version: 1, id: p.id, token: p.token });
      }
      if (pathname.endsWith("/sync")) return state.syncError ? error(state.syncError) : json({ id: `sync-${body.peer}-${body.provider}` });
      if (pathname.endsWith("/remove")) state.peers = state.peers.filter((p) => p.id !== body.id);
      return json({ ok: true });
    }
    if (pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, pathname === "/" ? "index.html" : pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    return route.fulfill({ body: await fs.readFile(file), contentType });
  };
  return state;
}

// A real wheel moves the dialog; the app correctly rejects a browser's
// unsolicited scrollIntoView, including the one Playwright tries on click.
async function reach(page, target) {
  await target.waitFor({ state: "visible" });
  for (let i = 0; i < 60; i++) {
    const pos = await target.evaluate((e) => {
      const r = e.getBoundingClientRect(), body = e.closest(".ebody"), b = body?.getBoundingClientRect();
      return { r: { top: r.top, bottom: r.bottom }, b: b && { top: b.top, bottom: b.bottom, x: b.x, width: b.width }, hit: e.contains(document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2)) };
    });
    if (!pos.b || pos.hit && pos.r.top >= pos.b.top && pos.r.bottom <= pos.b.bottom) return;
    await page.mouse.move(pos.b.x + pos.b.width / 2, (pos.b.top + pos.b.bottom) / 2);
    await page.mouse.wheel(0, pos.r.top < pos.b.top ? -120 : 120);
    await page.waitForFunction(() => document.querySelector("#modal .ebody"), undefined, { polling: "raf" });
    // Let the wheel's async compositing settle; assertions never depend on this delay.
    await page.waitForTimeout(40);
  }
  throw new Error("device editor control did not come into view");
}

for (const engine of process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"]) {
  for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) for (const width of [900, 440]) {
    test(`${engine} ${lang} ${width}px: pair, select, sync and remove devices`, { timeout: 90000 }, async (t) => {
      const tr = (key) => dict[lang]?.[key] || key;
      const browser = await (engine === "webkit" ? webkit : chromium).launch();
      t.after(() => browser.close());
      const page = await (await browser.newContext({ viewport: { width, height: 800 }, reducedMotion: "reduce" })).newPage();
      page.setDefaultTimeout(5000);
      const state = fixture(lang), errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await page.route("**/*", state.serve);
      await page.goto("http://peer-ui.test/?view=settings&tab=sync");
      const dialog = page.getByRole("dialog");
      const b = (key) => dialog.getByRole("button", { name: tr(key), exact: true });
      const label = (key) => dialog.getByLabel(tr(key), { exact: true });
      const fill = async (key, value) => { await reach(page, label(key)); await label(key).fill(value); };
      const tick = async (key) => { await reach(page, label(key)); await label(key).check(); };
      await page.locator("#managePeers").click();
      const helpWidth = await dialog.locator(".ebody > p").first().evaluate((e) => e.getBoundingClientRect().width / e.parentElement.getBoundingClientRect().width);
      assert.ok(helpWidth > .8, "pairing explanation was squeezed beside the device list");
      await b("Create pairing").click();
      await b("Back").click();
      assert.equal(state.peers.length, 0, "canceling a draft saved a link");
      await b("Create pairing").click();
      await label("Device name").waitFor();
      assert.equal(await dialog.locator('input[type="checkbox"]:checked').count(), 1, "new pairing granted sharing or key access");
      await fill("Device name", "Office laptop");
      await tick("Use this device's shared models");
      const api = dialog.locator('[data-provider="api"]');
      await reach(page, api.locator("summary"));
      await api.locator("summary").click();
      const pick = async (key, model) => {
        const c = api.getByRole("checkbox", { name: tr(key).replace("{model}", model), exact: true });
        await reach(page, c); await c.check();
      };
      await pick("Share calls: {model}", "model-one");
      await pick("Copy config: {model}", "model-two");
      await tick("Include API keys and credential headers");
      const claude = dialog.locator('[data-provider="claude"]');
      await reach(page, claude.locator("summary"));
      await claude.locator("summary").click();
      assert.equal(await claude.locator('input[type="checkbox"]').last().isDisabled(), true, "native configuration copying offered");
      await fill("Allowed source provider IDs", "other-api, other-api");
      await tick("Allow receiving API keys and credential headers");
      if (process.env.ARTIFACT_DIR && lang === "zh") {
        await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `peer-editor-${engine}-${width}.png`) });
      }
      await b("Save").click();
      await label("Pairing code").waitFor();
      const paired = JSON.parse(await label("Pairing code").inputValue());
      assert.equal(paired.id, "office");
      assert.ok(paired.token.startsWith("test-only-"));
      assert.equal(state.peers.length, 1);
      const saved = state.peers[0];
      assert.deepEqual(saved.exports, [{ provider: "api", models: ["model-one"] }]);
      assert.deepEqual(saved.syncSend, [{ provider: "api", models: ["model-two"], keys: true }]);
      assert.deepEqual(saved.syncReceive, ["other-api"]);
      assert.equal(saved.receiveKeys, true);
      assert.equal(saved.import, true);
      assert.equal(state.writes.filter((r) => r.pathname.endsWith("/sync")).length, 0, "saving configuration sent it automatically");
      await b("Back").click();
      await dialog.locator('[data-peer="office"]').waitFor();
      assert.equal((await dialog.textContent()).includes(paired.token), false, "list showed a pairing secret");
      await b("Edit").click();
      await fill("Device name", "Renamed laptop");
      state.saveError = "Device settings changed; reopen the device before saving";
      await b("Save").click();
      await page.waitForFunction(() => document.querySelector(".peer-editor .editor-error")?.textContent);
      assert.equal(await label("Device name").inputValue(), "Renamed laptop", "failed save dropped the draft");
      state.saveError = "";
      await b("Save").click();
      await dialog.locator('[data-peer="office"]').waitFor();
      assert.equal(state.peers[0].token, paired.token);
      const editWrite = state.writes.filter((r) => r.pathname.endsWith("/save")).at(-1).body;
      assert.equal(editWrite.peer.token, "");
      assert.ok(editWrite.revision);
      assert.deepEqual(editWrite.peer.exports, saved.exports);
      await b("Sync providers…").click();
      state.syncError = "receiver denied this provider";
      await b("Sync now").click();
      await page.waitForFunction(() => document.querySelector(".peer-editor .editor-error")?.textContent.includes("receiver denied"));
      assert.equal(await b("Synced").count(), 0);
      state.syncError = "";
      await b("Sync now").click();
      await b("Synced").waitFor();
      assert.deepEqual(state.writes.filter((r) => r.pathname.endsWith("/sync")).at(-1).body, { peer: "office", provider: "api", revision: state.peers[0].revision });
      await b("Back").click();
      await b("Join pairing").click();
      await fill("Pairing code", "bad code");
      await b("Save").click();
      await page.waitForFunction(() => document.querySelector(".peer-editor .editor-error")?.textContent);
      assert.equal(state.peers.length, 1);
      await fill("Pairing code", JSON.stringify({ ...paired, id: "second", exports: ["must be ignored"] }));
      await fill("Other computer's address", "wss://office.example.com");
      await b("Save").click();
      await dialog.locator('[data-peer="second"]').waitFor();
      assert.equal(state.peers[1].url, "wss://office.example.com");
      assert.deepEqual(state.peers[1].exports, []);
      assert.equal(state.peers[1].receiveKeys, false);
      await dialog.locator('[data-peer="office"]').getByRole("button", { name: tr("Edit"), exact: true }).click();
      await b("Remove device…").click();
      assert.equal(state.peers.length, 2, "remove happened without its final button");
      await b("Remove").click();
      await page.waitForFunction(() => document.querySelectorAll(".peer-device").length === 1);
      assert.equal(state.peers[0].id, "second");
      assert.deepEqual(errors, []);
      const visible = await dialog.locator(".peer-device").evaluate((e) => {
        const r = e.getBoundingClientRect(), b = e.closest(".ebody").getBoundingClientRect();
        return r.top >= b.top && r.bottom <= b.bottom;
      });
      assert.equal(visible, true, "single device was clipped inside a collapsed loading dialog");
      const overflow = await dialog.evaluate((d) => d.scrollWidth - d.clientWidth);
      assert.ok(overflow <= 1, `dialog overflowed by ${overflow}px`);
      if (process.env.ARTIFACT_DIR && lang === "zh" && width === 900) {
        await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `peer-devices-${engine}.png`) });
      }
    });
  }
  test(`${engine}: unreadable configuration and offline gateway stay distinct`, async (t) => {
    const browser = await (engine === "webkit" ? webkit : chromium).launch();
    t.after(() => browser.close());
    const page = await browser.newPage({ viewport: { width: 900, height: 800 } });
    const state = fixture("en");
    state.readError = "unreadable peer configuration";
    await page.route("**/*", state.serve);
    await page.goto("http://peer-ui.test/?view=settings&tab=sync");
    await page.locator("#managePeers").click();
    const dialog = page.getByRole("dialog");
    await page.waitForFunction(() => document.querySelector(".peer-editor .editor-error")?.textContent);
    assert.equal(await dialog.getByRole("button", { name: "Create pairing", exact: true }).isDisabled(), true);
    assert.equal((await dialog.textContent()).includes("No devices paired"), false);
    state.readError = "";
    state.statusError = "Cannot reach the local gateway; start it and refresh";
    await dialog.getByRole("button", { name: "Refresh", exact: true }).click();
    await page.waitForFunction(() => document.querySelector(".peer-editor .editor-error")?.textContent.includes("Cannot reach"));
    assert.equal(await dialog.getByRole("button", { name: "Create pairing", exact: true }).isDisabled(), false);
  });
}
