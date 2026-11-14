// Key allowance cards use the enabled key's name (or mask); a provider
// with just one enabled key has an unnamed card. Match that contract,
// never the order of the replies or a provider-wide card in a multi-key pool.
const rows = new WeakMap();
function keyUser(p, k) {
  const on = p.keyList.filter(x => x.on);
  if (!k.on) return null;
  if (on.length === 1) return "";
  const user = k.name || k.masked;
  return on.filter(x => (x.name || x.masked).toLowerCase() === user.toLowerCase()).length === 1 ? user : null;
}

function paint(row, entry) {
  const { p, user, mode } = entry;
  const cards = (quotas || []).filter(q => q.provider === p.id && (q.user || "").toLowerCase() === user.toLowerCase());
  const q = cards.length === 1 ? cards[0] : undefined;
  const meter = accountQuota(quotas === null ? null : { [user]: q }, user);
  // Keep controls, focus and typed edits in place when a late read arrives.
  const old = row.querySelector(":scope > .aq"), body = row.closest(".ebody"), top = body?.scrollTop;
  if (old) old.replaceWith(meter); else row.append(meter);
  if (body) body.scrollTop = top;
  mode.textContent = t(quotaLeft ? "Left" : "Used");
  mode.title = t(quotaLeft ? "Show how much of each window is used" : "Show how much of each window is left");
  mode.hidden = !q?.windows?.length || !!q.error;
}

// Called by the existing quota renderer: reads, individual refreshes and
// used/left changes update only the meters, including a delayed GET reply.
window.renderKeyQuotas = function () {
  for (const row of document.querySelectorAll(".accts.keys .acc[data-account-id]")) {
    const entry = rows.get(row);
    if (entry) paint(row, entry);
  }
};

export function keyQuotaFill(row, p, k) {
  if (!k.usageCap || rows.has(row)) return;
  const user = keyUser(p, k);
  // Disabled keys aren't read by the quota API. Ambiguous labels mustn't
  // copy another key's allowance or refresh several accounts as one.
  if (user === null) return;
  row.classList.add("with-aq");
  const refresh = quotaRefresh({ provider: p.id, user });
  refresh.classList.remove("reading");
  refresh.classList.add("key-quota-refresh");
  refresh.title = t("Read this account's usage again");
  const mode = el("button", "text quiet key-quota-mode");
  mode.type = "button";
  mode.onclick = e => { e.stopPropagation(); setQuotaLeft(!quotaLeft); };
  (row.querySelector(":scope > .plan") || row.querySelector(":scope > .n")).after(refresh, mode);
  const entry = { p, user, mode };
  rows.set(row, entry);
  paint(row, entry);
  // Opening the editor reads cached cards, respecting quotaReads=asked.
  // Only pressing the account's refresh explicitly reads that account.
  if (!quotas || Date.now() - quotasAt >= 60000) loadQuotas();
}
