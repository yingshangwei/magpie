// Device links live beside backup settings, but permissions and pairing
// secrets stay on this computer. Reading this page never starts a sync.
(() => {
  const button = (key, run, cls = "text") => {
    const b = el("button", cls, t(key));
    b.type = "button";
    b.onclick = run;
    return b;
  };
  const check = (key, value, parent) => {
    const label = el("label", "peer-check"), box = el("input");
    box.type = "checkbox";
    box.checked = !!value;
    label.append(box, el("span", "", t(key)));
    parent.append(label);
    return box;
  };
  const field = (key, value, parent, multiline = false) => {
    const label = el("label", "peer-field"), control = el(multiline ? "textarea" : "input", "input");
    control.value = value || "";
    control.setAttribute("aria-label", t(key));
    control.spellcheck = false;
    control.autocomplete = "off";
    if (multiline) control.rows = 3;
    label.append(el("span", "", t(key)), control);
    parent.append(label);
    return control;
  };
  const shell = (title, back) => {
    const ed = el("div", "editor framed peer-editor"), head = el("div", "ehead"), body = el("div", "ebody"), foot = el("div", "bar");
    head.append(el("b", "", t(title)));
    foot.append(button(back ? "Back" : "Close", back || closeModal));
    ed.append(head, body, foot);
    return { ed, body, foot };
  };
  const open = (ed) => { openModal(ed); markModalSaved(); };
  const note = (body, key) => body.append(el("p", "sub wraps", t(key)));
  const errorBox = (body) => { const n = el("p", "editor-error"); n.setAttribute("role", "status"); body.append(n); return n; };
  const busy = async (b, err, run) => {
    if (b.disabled) return;
    b.disabled = true;
    b.classList.add("busy");
    err.textContent = "";
    try { await run(); } catch (e) { err.textContent = t(e.message); }
    finally { b.disabled = false; b.classList.remove("busy"); }
  };
  const stateName = (p, state) => {
    if (p.off) return t("Disabled");
    if (!state) return t("Connection status unavailable");
    const refused = /^connection refused \((\d+)\)$/.exec(state.status);
    if (refused) return t("Connection refused ({code})", { code: refused[1] });
    return t({ connected: "Connected", "waiting for connection": "Waiting for connection", disabled: "Disabled",
      connecting: "Connecting…", "connection failed": "Connection failed; retrying",
      "protocol handshake failed": "Pairing handshake failed",
      "configuration changed during handshake": "Device settings changed; reconnecting",
      "connected; catalog refresh failed": "Connected; model list refresh failed" }[state.status] || state.status);
  };

  async function manage() {
    const { ed, body, foot } = shell("Device links");
    note(body, "Use remote models without copying keys. Configuration sync is a separate, manual action.");
    const err = errorBox(body), list = el("div", "peer-devices");
    body.append(list);
    let data;
    const refresh = button("Refresh", () => busy(refresh, err, load));
    const add = button("Create pairing", () => busy(add, err, async () => {
      const draft = await api("peers/draft", {});
      edit({ id: draft.id, token: draft.token }, data.providers, "create");
    }));
    const join = button("Join pairing", () => edit({}, data.providers, "join"));
    add.disabled = join.disabled = true;
    foot.prepend(add, join, refresh);
    const load = async () => {
      add.disabled = join.disabled = true;
      data = await api("peers");
      if (!Array.isArray(data.peers) || !Array.isArray(data.providers)) throw new Error("Invalid device settings");
      add.disabled = join.disabled = false;
      let states = [];
      try { states = await api("peers/status"); if (!Array.isArray(states)) throw new Error("Invalid local gateway response"); }
      catch (e) { err.textContent = t(e.message); }
      list.replaceChildren();
      if (!data.peers.length) note(list, "No devices paired. Create a pairing here, then join it on the other computer.");
      for (const p of data.peers) {
        const row = el("div", "row pref peer-device"), who = el("div", "who"), tools = el("div", "val");
        row.dataset.peer = p.id;
        who.append(el("div", "name", p.name || p.id), el("div", "sub wraps", stateName(p, states.find((s) => s.id === p.id))),
          el("div", "sub wraps", p.url || t("Waiting for the other computer to connect")));
        tools.append(button("Edit", () => edit(p, data.providers)), button("Pairing code", (e) => busy(e.currentTarget, err, async () => pairing(await api("peers/pairing", { id: p.id, revision: p.revision })))));
        const refreshModels = button("Refresh models", (e) => busy(e.currentTarget, err, async () => {
          await api("peers/refresh", { peer: p.id });
          await load();
        }));
        refreshModels.disabled = p.off || !p.import;
        tools.append(refreshModels);
        if (p.syncSend?.length) tools.append(button("Sync providers…", () => sync(p)));
        row.append(who, tools);
        list.append(row);
      }
    };
    await busy(refresh, err, load);
    open(ed);
  }

  function pairing(code) {
    const { ed, body } = shell("Pairing code", manage);
    note(body, "On the other computer, open Device links → Join pairing and paste this code. Keep it private.");
    const value = JSON.stringify(code), output = field("Pairing code", value, body, true);
    output.readOnly = true;
    body.append(copyBtn(value, t("Pairing code")));
    note(body, "Only one computer enters the other's address. At least one must be reachable; a public relay is not included.");
    open(ed);
  }

  function edit(p, providers, mode = "edit") {
    const { ed, body, foot } = shell(mode === "edit" ? "Edit device" : mode === "join" ? "Join pairing" : "Create pairing", manage);
    const name = field("Device name", p.name, body);
    let code;
    if (mode === "join") code = field("Pairing code", "", body, true);
    if (mode !== "join") body.append(el("p", "sub wraps", t("Link ID: {id}", { id: p.id })));
    const address = field("Other computer's address", p.url, body);
    address.placeholder = "wss://gateway.example.com";
    note(body, "Leave the address empty to wait for the other computer. Fill it on only one side. Network connections require wss://; ws:// works only for a local tunnel.");
    const enabled = check("Enable this link", !p.off, body);
    const importing = check("Use this device's shared models", p.import, body);
    note(body, "Models shared by this device appear as a separate provider after connecting. Its keys stay there.");

    const selections = new Map();
    // Kept selections remain editable even if a provider or model is now
    // off or absent from the current catalog. Never silently drop them.
    const choices = providers.map((v) => ({ ...v, models: [...v.models] }));
    for (const s of [...(p.exports || []), ...(p.syncSend || [])]) {
      let v = choices.find((v) => v.id === s.provider);
      if (!v) choices.push(v = { id: s.provider, name: s.provider, models: [], canSync: (p.syncSend || []).some((x) => x.provider === s.provider) });
      for (const model of s.models) if (!v.models.includes(model)) v.models.push(model);
    }
    body.append(el("h3", "", t("Share from this computer")));
    note(body, "Shared calls use this computer's keys. Copying configuration creates an API provider on the other computer. Native Claude and Codex configurations cannot be copied.");
    if (!choices.length) note(body, "No local models available. Add and enable a provider first.");
    for (const v of choices) {
      const group = el("details", "peer-models"), summary = el("summary", "", v.name || v.id);
      group.dataset.provider = v.id;
      const shared = new Set(p.exports?.find((s) => s.provider === v.id)?.models || []);
      const copied = p.syncSend?.find((s) => s.provider === v.id);
      summary.append(el("span", "sub", " · " + v.id));
      group.append(summary);
      const grid = el("div", "peer-model-grid");
      grid.append(el("span", "", t("Model")), el("span", "", t("Share calls")), el("span", "", t("Copy config")));
      const models = [];
      for (const model of v.models) {
        const share = el("input"), copy = el("input");
        share.type = copy.type = "checkbox";
        share.checked = shared.has(model);
        copy.checked = !!copied?.models.includes(model);
        copy.disabled = !v.canSync && !copy.checked;
        share.setAttribute("aria-label", t("Share calls: {model}", { model }));
        copy.setAttribute("aria-label", t("Copy config: {model}", { model }));
        grid.append(el("span", "peer-model-name", model), share, copy);
        models.push({ model, share, copy });
      }
      group.append(grid);
      const keys = v.canSync || copied ? check("Include API keys and credential headers", copied?.keys, group) : null;
      if (!v.canSync && !copied) note(group, "This provider supports shared calls only");
      if (keys) note(group, "Keys are sent only during Sync now, and the other device must allow receiving them.");
      selections.set(v.id, { models, keys });
      body.append(group);
    }
    body.append(el("h3", "", t("Receive configuration")));
    const receive = field("Allowed source provider IDs", (p.syncReceive || []).join(", "), body);
    note(body, "Enter exact provider IDs from the other computer, separated by commas. Empty allows no configuration copies.");
    const receiveKeys = check("Allow receiving API keys and credential headers", p.receiveKeys, body);
    note(body, "Received configurations choose API addresses, including private addresses. Copies without API keys stay disabled.");
    const err = errorBox(body);
    const save = button("Save", () => busy(save, err, async () => {
      let link = { id: p.id, token: p.token || "" };
      if (mode === "join") {
        try {
          const parsed = JSON.parse(code.value.trim());
          if (parsed.version !== 1 || typeof parsed.id !== "string" || typeof parsed.token !== "string") throw new Error();
          link = { id: parsed.id, token: parsed.token };
        } catch { throw new Error("Invalid pairing code"); }
      }
      const exports = [], syncSend = [];
      for (const [provider, s] of selections) {
        const shared = s.models.filter((m) => m.share.checked).map((m) => m.model);
        const copied = s.models.filter((m) => m.copy.checked).map((m) => m.model);
        if (shared.length) exports.push({ provider, models: shared });
        if (copied.length) syncSend.push({ provider, models: copied, keys: !!s.keys?.checked });
      }
      await api("peers/save", { revision: p.revision || "", peer: { ...link, name: name.value.trim(), url: address.value.trim(), off: !enabled.checked,
        import: importing.checked, exports, syncSend, syncReceive: [...new Set(receive.value.split(/[\s,，]+/).filter(Boolean))], receiveKeys: receiveKeys.checked } });
      markModalSaved();
      if (mode === "create") pairing({ version: 1, ...link });
      else await manage();
    }), "primary");
    foot.prepend(save);
    if (mode === "edit") foot.append(button("Remove device…", () => remove(p)));
    open(ed);
  }

  function remove(p) {
    const { ed, body, foot } = shell("Remove device?", manage);
    body.append(el("p", "", p.name || p.id));
    note(body, "This closes the link and stops shared calls. API configurations already copied remain on each computer.");
    const err = errorBox(body), remove = button("Remove", () => busy(remove, err, async () => {
      await api("peers/remove", { id: p.id, revision: p.revision });
      await manage();
    }));
    foot.prepend(remove);
    open(ed);
  }

  function sync(p) {
    const { ed, body } = shell("Sync providers…", manage);
    note(body, "Each button sends only that provider's selected models and chosen credentials. The receiver must allow its source provider ID.");
    const err = errorBox(body);
    for (const s of p.syncSend) {
      const row = el("div", "row pref"), who = el("div", "who");
      who.append(el("div", "name", s.provider), el("div", "sub wraps", s.models.join(", ")),
        el("div", "sub wraps", t(s.keys ? "Includes API keys and credential headers" : "Without keys; the received copy stays disabled")));
      const send = button("Sync now", () => busy(send, err, async () => {
        const result = await api("peers/sync", { peer: p.id, provider: s.provider, revision: p.revision });
        err.textContent = "";
        send.textContent = t("Synced");
        who.append(el("div", "sub wraps", t("Saved on the other device as {id}", { id: result.id })));
      }));
      send.disabled = !!p.off;
      row.append(who, send);
      body.append(row);
    }
    open(ed);
  }
  $("#managePeers").onclick = manage;
})();
