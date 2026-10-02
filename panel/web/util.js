// 工具函数与 API 客户端（无第三方依赖）

export const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
export const attr = esc;

export const num = (v) => {
  if (v === null || v === undefined || v === '' || Number.isNaN(Number(v))) return '—';
  return Number(v).toLocaleString('zh-CN');
};
export const flt = (v, d = 1) => {
  if (v === null || v === undefined || v === '' || Number.isNaN(Number(v))) return '—';
  const n = Number(v);
  return n.toFixed(d).replace(/\.0+$/, (m) => (d > 0 ? '' : m));
};
export const pct = (v, d = 1) => {
  if (v === null || v === undefined || v === '' || Number.isNaN(Number(v))) return '—';
  return (Number(v) * 100).toFixed(d) + '%';
};
export const bytes = (n) => {
  n = Number(n) || 0;
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB';
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB';
};
export const dur = (sec) => {
  sec = Math.max(0, Math.round(Number(sec) || 0));
  if (sec < 60) return sec + ' 秒';
  const m = Math.floor(sec / 60), s = sec % 60;
  if (m < 60) return m + ' 分' + (s ? ' ' + s + ' 秒' : '');
  const h = Math.floor(m / 60), mm = m % 60;
  if (h < 24) return h + ' 小时' + (mm ? ' ' + mm + ' 分' : '');
  return Math.floor(h / 24) + ' 天' + (h % 24 ? ' ' + (h % 24) + ' 小时' : '');
};
export const uptime = (sec) => dur(sec);

export function parseTime(v) {
  if (!v) return null;
  if (typeof v === 'number') return new Date(v < 1e12 ? v * 1000 : v);
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d;
}
export function ts(v) {
  const d = parseTime(v);
  if (!d) return '—';
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
export function tsShort(v) {
  const d = parseTime(v);
  if (!d) return '—';
  const p = (n) => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
export function ago(v) {
  const d = parseTime(v);
  if (!d) return '—';
  let s = (Date.now() - d.getTime()) / 1000;
  if (s < 0) s = 0;
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}

export function uid8(uid) {
  if (!uid) return '';
  return String(uid).replace(/-/g, '').slice(0, 8);
}

/** 把任意值渲染成紧凑的「键: 值」片段（用于 realm_totals / daily_budget 这类裸 JSON）。 */
export function inlineKV(v) {
  if (v === null || v === undefined || v === '') return '';
  if (typeof v !== 'object') return esc(v);
  const parts = [];
  for (const [k, val] of Object.entries(v)) {
    if (val === null || val === undefined || val === '') continue;
    parts.push(`${esc(k)}=${esc(typeof val === 'object' ? JSON.stringify(val) : val)}`);
  }
  return parts.join(' · ');
}
export function objKV(v) {
  if (!v || typeof v !== 'object') return [];
  return Object.entries(v).filter(([, val]) => val !== null && val !== undefined && val !== '' && !(typeof val === 'object' && !Array.isArray(val) && Object.keys(val).length === 0));
}

async function req(method, url, body, opts = {}) {
  const init = { method, credentials: 'same-origin', headers: {}, ...opts };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(url, init);
  } catch (e) {
    throw new Error('网络请求失败：' + (e && e.message ? e.message : e));
  }
  const ct = res.headers.get('content-type') || '';
  let data = null;
  if (ct.includes('json')) {
    data = await res.json().catch(() => null);
  } else {
    const text = await res.text().catch(() => '');
    data = { ok: res.ok, text };
  }
  if (!res.ok || (data && data.ok === false)) {
    const msg = (data && (data.error || (data.error && data.error.message))) || (data && data.text) || `HTTP ${res.status}`;
    const err = new Error(typeof msg === 'string' ? msg : JSON.stringify(msg));
    err.status = res.status;
    err.payload = data;
    throw err;
  }
  return data || {};
}

export const api = {
  get: (u, o) => req('GET', u, undefined, o),
  post: (u, b, o) => req('POST', u, b === undefined ? {} : b, o),
  put: (u, b, o) => req('PUT', u, b, o),
  del: (u, o) => req('DELETE', u, undefined, o),
};

/** 简单并发：同时跑多个 promise，各自失败返回 {error}。 */
export async function all(pairs) {
  const keys = Object.keys(pairs);
  const vals = await Promise.all(keys.map((k) => Promise.resolve(pairs[k]).catch((e) => ({ __error: e.message || String(e) }))));
  const out = {};
  keys.forEach((k, i) => { out[k] = vals[i]; });
  return out;
}
export const isErr = (v) => v && typeof v === 'object' && typeof v.__error === 'string';
