// Shared API helpers for the analytics tabs. Auth follows the same
// convention as the overview: the CPA management key is read from
// same-origin localStorage; a missing key or a 401/403 response throws
// Error("key-prompt"), other HTTP failures throw Error("http:<status>").

export const API_BASE = "/v0/management/plugins/cpa-token-analytics/api";
export const KEY_PROMPT = "key-prompt";

function xorBytes(bytes, keyBytes) {
  const output = new Uint8Array(bytes.length);
  for (let i = 0; i < bytes.length; i++) {
    output[i] = bytes[i] ^ keyBytes[i % keyBytes.length];
  }
  return output;
}

// Read the CPA management panel session from same-origin localStorage,
// following the secure-storage format used across the CPA plugin ecosystem.
function decodeCPAStorage(raw) {
  if (!raw) return null;
  const prefix = "enc::v1::";
  let text = raw;
  if (text.startsWith(prefix)) {
    const binary = atob(text.slice(prefix.length));
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    const storageKey = `cli-proxy-api-webui::secure-storage|${location.host}|${navigator.userAgent}`;
    text = new TextDecoder().decode(
      xorBytes(bytes, new TextEncoder().encode(storageKey)),
    );
  }
  return JSON.parse(text);
}

export function readManagementKey() {
  try {
    const parsed = decodeCPAStorage(localStorage.getItem("cli-proxy-auth"));
    const state = parsed && parsed.state ? parsed.state : parsed;
    const value = state && state.managementKey;
    return typeof value === "string" ? value.trim() : "";
  } catch {
    return "";
  }
}

export function buildHeaders() {
  const key = readManagementKey();
  if (!key) throw Error(KEY_PROMPT);
  return { Authorization: `Bearer ${key}` };
}

export async function fetchJSON(path, { method = "GET", body, signal } = {}) {
  const r = await fetch(`${API_BASE}${path}`, {
    method,
    signal,
    headers: {
      ...buildHeaders(),
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
    },
    ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
  });
  if (r.status === 401 || r.status === 403) throw Error(KEY_PROMPT);
  if (!r.ok) throw Error(`http:${r.status}`);
  const d = await r.json();
  if (d && d.error) throw Error(d.error);
  return d;
}

function qs(params) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v !== undefined && v !== null && v !== "") q.set(k, v);
  }
  const s = q.toString();
  return s ? `?${s}` : "";
}

export function fetchAnalytics(name, params) {
  return fetchJSON(`/analytics/${name}${qs(params)}`);
}

export function fetchPricing(path, opts) {
  return fetchJSON(`/pricing/${path}`, opts);
}

export function fetchFx(path, opts) {
  return fetchJSON(`/fx/${path}`, opts);
}

// Requests inspector endpoints: GET /api/requests (cursor-paginated list)
// and GET /api/requests/:id (detail). `params` is the object built by
// buildRequestParams in src/requests.js.
export function fetchRequests(params, { signal } = {}) {
  return fetchJSON(`/requests${qs(params)}`, { signal });
}

export function fetchRequest(id, { signal } = {}) {
  return fetchJSON(`/requests/${encodeURIComponent(id)}`, { signal });
}
