// The upstream UI expects a nonempty key in its persisted connection state.
// This public marker is NOT a credential: the server authorizes the socket peer
// and browser origin in local mode, and rejects this marker in normal key mode.
(() => {
  const apiBase = window.location.origin;
  const managementKey = "loopback-local-management";
  const state = { apiBase, managementKey, rememberPassword: true };
  localStorage.setItem("cli-proxy-auth", JSON.stringify({ state, version: 0 }));
  localStorage.setItem("apiBase", JSON.stringify(apiBase));
  localStorage.setItem("apiUrl", JSON.stringify(apiBase));
  localStorage.setItem("managementKey", JSON.stringify(managementKey));
  localStorage.setItem("isLoggedIn", "true");
})();
