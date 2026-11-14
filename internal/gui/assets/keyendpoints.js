// A custom provider's keys may reach different sites. Clearing the override
// follows the provider again; changing sites also clears its model-list URL.
window.accountEndpoint = function (p, k, button) {
  const apiName = baseAPIOf(p), protocol = apiName === "openai" ? "chat" : apiName;
  const own = p.accountEndpoints?.[k.id];
  const base = (own || p)[protocol] || "";
  button.disabled = false;
  button.type = "button";
  button.classList.toggle("set", !!own);
  button.title = `${t("Base URL")}: ${base}`;
  button.onclick = () => renameIn(button, own?.[protocol] || "", base, (value) => {
    value = value.trim();
    const endpoints = value ? { ...(own || {}), modelsURL: "", [protocol]: value } : {};
    return accountAction("keys/endpoints", { id: p.id, ref: k.id, endpoints });
  });
  return button;
};
