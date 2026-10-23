// A desktop window beside a background gateway must still offer its routing
// groups and account settings. Only the live trace belongs to the process
// serving the gateway. All requests here are isolated API and asset fixtures.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const models = [
  { id: "claude/opus", name: "Opus", providerName: "Claude Code", icon: "generic" },
  { id: "relay/opus", name: "Opus", providerName: "Relay", icon: "generic" },
];
const group = { id: "auto-opus", name: "Opus together", auto: true, ready: true,
  members: models.map((m) => m.id), memberInfo: models.map((m) => ({ id: m.id, ready: true })) };
const pool = { provider: "claude", name: "Claude Code", kind: "account", who: ["one@example.test", "two@example.test"], routing: "", affinity: "" };
const cases = {
  "another window": { running: true, window: true },
  "background service": { running: true, window: false },
  "stopped gateway": { running: false },
};

function fixture(lang, gateway, own = false) {
  const f = { own, posts: [], waits: [] };
  f.waiting = new Promise((resolve) => { f.waitStarted = resolve; });
  const data = () => ({ models, groups: [group], pools: [pool], found: true });
  const recorded = { id: 1, seq: 1, time: new Date().toISOString(), agent: "claude",
    model: "claude/opus", provider: "claude", done: true, status: 200, ms: 100, tokens: 10,
    order: [], tries: [] };
  f.route = async (r) => {
    const req = r.request(), url = new URL(req.url());
    const json = (value) => r.fulfill({ json: value });
    if (url.pathname === "/boot.js") return r.fulfill({ contentType: "text/javascript", body: `window.bootPrefs=${JSON.stringify({ lang, theme: "light", web: true })};` });
    if (url.pathname === "/wails/runtime.js") return r.fulfill({ contentType: "text/javascript", body: "export const Window={};" });
    if (url.pathname === "/api/state") return json({ agents: [{ id: "claude", name: "Claude Code", fields: [] }], profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway });
    if (url.pathname === "/api/gateway/trace") {
      const reply = () => json({ mine: f.own, now: new Date().toISOString(), seq: 1,
        totals: { requests: own ? 1 : 0 }, routes: f.own ? [recorded] : [] });
      if (url.searchParams.has("wait")) return new Promise((resolve) => {
        f.waits.push(() => resolve(reply()));
        f.waitStarted();
      });
      return reply();
    }
    if (url.pathname === "/api/gateway/history") return json({ cut: false, days: [], routes: [] });
    if (url.pathname === "/api/groups") return json(data());
    if (url.pathname === "/api/groups/save") {
      f.posts.push(req.postDataJSON());
      return json(data());
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    const body = await fs.readFile(file).catch(() => null);
    return body ? r.fulfill({ body, contentType }) : r.fulfill({ status: 404, body: "" });
  };
  return f;
}

async function open(engine, lang, width, f, t) {
  const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
  const page = await browser.newPage({ viewport: { width, height: 800 }, reducedMotion: "reduce" });
  t.after(async () => {
    for (const release of f.waits.splice(0)) await release();
    await browser.close();
  });
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.route("**/*", f.route);
  await page.goto("http://magpie.test/?view=routing");
  await page.locator('.rt-group[data-id="auto-opus"]').waitFor({ state: "attached" });
  return { page, errors };
}

for (const engine of process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"]) {
  for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
    for (const width of [1000, 420]) {
      for (const [scene, gateway] of Object.entries(cases)) {
        test(`${engine} ${lang} ${width}px: ${scene} keeps routing groups and accounts editable`, async (t) => {
          const f = fixture(lang, gateway);
          const { page, errors } = await open(engine, lang, width, f, t);
          await page.locator(".rt-off").waitFor({ state: "visible" });
          const row = page.locator('.rt-group[data-id="auto-opus"]');
          assert(await row.isVisible(), "routing groups must stay visible without this window's live trace");
          assert(await page.locator('.rt-pool[data-provider="claude"]').isVisible(), "native account routing remains available");
          assert.equal(await page.locator(".rt-stage").isVisible(), false);
          assert.equal(await page.locator(".rt-cols").isVisible(), false);

          await row.locator("button.text").click();
          const editor = page.locator(".rt-gedit");
          await editor.waitFor();
          assert.equal(await editor.locator("input").first().inputValue(), group.name);
          assert.deepEqual(await editor.locator(".fbrow .n small").allTextContents(), ["Claude Code", "Relay"]);
          assert(await editor.evaluate((e) => e.getBoundingClientRect().right <= innerWidth + 1), "the editor fits the window");
          assert.deepEqual(f.posts, [], "opening a group does not save it");
          await editor.locator("input").first().press("Escape");
          await row.waitFor();
          await page.locator("#rtNewGroup").click();
          await editor.waitFor();
          assert.equal(await editor.locator("input").first().inputValue(), "", "New group is usable beside an external gateway");
          assert.deepEqual(errors, []);
        });
      }
    }
  }

  test(`${engine}: groups remain usable when gateway ownership changes`, async (t) => {
    const f = fixture("en", cases["another window"], true);
    const { page, errors } = await open(engine, "en", 1000, f, t);
    await page.locator(".rt-cols").waitFor({ state: "visible" });
    await f.waiting;
    f.own = false;
    for (const release of f.waits.splice(0)) await release();
    await page.locator(".rt-off").waitFor({ state: "visible" });
    const row = page.locator('.rt-group[data-id="auto-opus"]');
    assert(await row.isVisible(), "losing live trace ownership does not hide groups");
    // Returning to this page redraws its request history too; old requests
    // must not reappear as live while another process owns the gateway.
    await page.locator('#nav button[data-view="providers"]').click();
    await page.locator('#nav button[data-view="routing"]').click();
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await page.locator(".rt-cols").isVisible(), false, "a redraw keeps the unavailable trace hidden");
    assert(await row.isVisible());
    f.own = true;
    await page.locator(".rt-off").waitFor({ state: "hidden", timeout: 10000 });
    await page.locator(".rt-cols").waitFor({ state: "visible" });
    assert(await row.isVisible());
    assert.deepEqual(errors, []);
  });
}
