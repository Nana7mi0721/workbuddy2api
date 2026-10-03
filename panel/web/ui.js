// UI 原语：toast / 弹窗 / 确认框 / 页面骨架 / 事件委托
import { esc, api } from './util.js';

export function toast(msg, kind = 'ok', ms = 3200) {
  const root = document.getElementById('toasts');
  const el = document.createElement('div');
  el.className = 'toast' + (kind === 'ok' ? '' : ' ' + kind);
  el.innerHTML = `<div>${esc(msg)}</div>`;
  root.appendChild(el);
  setTimeout(() => {
    el.style.transition = 'opacity .25s, transform .25s';
    el.style.opacity = '0';
    el.style.transform = 'translateX(10px)';
    setTimeout(() => el.remove(), 260);
  }, ms);
}
export const toastErr = (e) => toast(typeof e === 'string' ? e : (e && e.message) || String(e), 'error', 5200);
export const toastWarn = (m) => toast(m, 'warn', 4200);

export function busy(el, on = true) {
  if (!el) return;
  if (on) {
    el.dataset.label = el.dataset.label || el.innerHTML;
    el.disabled = true;
    el.innerHTML = '<span class="spin"></span>处理中';
  } else {
    el.disabled = false;
    if (el.dataset.label) el.innerHTML = el.dataset.label;
  }
}

/** 通用弹窗：{title, body(html), footer(html)} → 返回 {el, close}。Esc 可关闭。 */
export function openModal({ title, body, footer = '', width }) {
  const root = document.getElementById('modal-root');
  const mask = document.createElement('div');
  mask.className = 'modal-mask';
  mask.innerHTML = `
    <div class="modal" ${width ? `style="width:min(${width},100%)"` : ''}>
      <header><h3>${esc(title)}</h3><div class="spacer"></div><button class="btn ghost sm" data-close>✕</button></header>
      <div class="body">${body}</div>
      ${footer ? `<footer>${footer}</footer>` : ''}
    </div>`;
  root.appendChild(mask);
  const onKey = (e) => { if (e.key === 'Escape') close(); };
  const close = () => {
    if (!mask.isConnected) return;
    mask.remove();
    document.removeEventListener('keydown', onKey);
  };
  document.addEventListener('keydown', onKey);
  mask.addEventListener('click', (e) => {
    if (e.target === mask || e.target.closest('[data-close]')) close();
  });
  return { el: mask, close };
}

export function confirmModal(title, text, { danger = false, okLabel = '确认' } = {}) {
  return new Promise((resolve) => {
    const m = openModal({
      title,
      body: `<div class="alert ${danger ? 'danger' : ''}">${text}</div>`,
      footer: `<button class="btn" data-no>取消</button><button class="btn ${danger ? 'danger' : 'primary'}" data-yes>${esc(okLabel)}</button>`,
      width: '460px',
    });
    m.el.addEventListener('click', (e) => {
      if (e.target.closest('[data-no]')) { m.close(); resolve(false); }
      if (e.target.closest('[data-yes]')) { m.close(); resolve(true); }
    });
  });
}

/** 需要用户输入一串文本才能继续（删除类操作）。 */
export function promptModal(title, label, expect, { danger = true, placeholder = '' } = {}) {
  return new Promise((resolve) => {
    const m = openModal({
      title,
      body: `<label class="field"><span>${label}</span><input type="text" id="pm-input" placeholder="${esc(placeholder)}" autocomplete="off"><em>请输入 <code>${esc(expect)}</code> 以确认</em></label>`,
      footer: `<button class="btn" data-no>取消</button><button class="btn ${danger ? 'danger' : 'primary'}" data-yes disabled>确认</button>`,
      width: '460px',
    });
    const input = m.el.querySelector('#pm-input');
    const yes = m.el.querySelector('[data-yes]');
    input.addEventListener('input', () => { yes.disabled = input.value.trim() !== expect; });
    input.focus();
    m.el.addEventListener('click', (e) => {
      if (e.target.closest('[data-no]')) { m.close(); resolve(false); }
      if (e.target.closest('[data-yes]')) { m.close(); resolve(true); }
    });
    input.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !yes.disabled) { m.close(); resolve(true); } });
  });
}

/** 事件委托：在容器上按 [data-act] 派发。handlers = {act: (el, ev) => {}} */
export function delegate(root, handlers) {
  root.addEventListener('click', (ev) => {
    const el = ev.target.closest('[data-act]');
    if (!el || !root.contains(el)) return;
    const fn = handlers[el.dataset.act];
    if (!fn) return;
    ev.preventDefault();
    fn(el, ev);
  });
  root.addEventListener('change', (ev) => {
    const el = ev.target.closest('[data-change]');
    if (!el || !root.contains(el)) return;
    const fn = handlers['@' + el.dataset.change];
    if (fn) fn(el, ev);
  });
}

export function copy(text, label = '已复制到剪贴板') {
  const done = () => toast(label);
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).then(done, () => fallback());
  } else fallback();
  function fallback() {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); done(); } catch (e) { toastErr('复制失败，请手动选择文本'); }
    ta.remove();
  }
}

export const badge = (text, kind = '') => `<span class="badge ${kind}">${esc(text)}</span>`;
export const dotBadge = (text, kind = '') => `<span class="badge ${kind}"><i class="dot"></i>${esc(text)}</span>`;

export function kpi(label, value, foot = '', act = '') {
  return `<div class="card kpi ${act ? 'clickable' : ''}" ${act ? `data-act="${act}"` : ''}>
    <div class="label">${esc(label)}</div>
    <div class="value">${value}</div>
    ${foot ? `<div class="foot">${foot}</div>` : ''}
  </div>`;
}

export function table(cols, rows, empty = '暂无数据') {
  if (!rows || !rows.length) return `<div class="table-wrap"><div class="empty">${esc(empty)}</div></div>`;
  const head = cols.map((c) => `<th class="${c.cls || ''}">${esc(c.label)}</th>`).join('');
  const body = rows.map((r) => `<tr>${r.map((c, i) => `<td class="${cols[i] && cols[i].cls || ''}">${c}</td>`).join('')}</tr>`).join('');
  return `<div class="table-wrap"><table><thead><tr>${head}</tr></thead><tbody>${body}</tbody></table></div>`;
}

export function statusBadge(code) {
  const c = Number(code);
  if (!c) return badge('—');
  if (c < 300) return badge(String(c), 'ok');
  if (c < 400) return badge(String(c), 'info');
  if (c < 500) return badge(String(c), 'warn');
  return badge(String(c), 'danger');
}

export function pageHead(title, sub, actions = '') {
  return `<div class="page-head"><div><h1>${esc(title)}</h1>${sub ? `<p>${sub}</p>` : ''}</div><div class="head-actions">${actions}</div></div>`;
}

/** 简易 JSON 视图（可折叠）。 */
export function jsonBlock(v) {
  return `<pre class="mono-block">${esc(JSON.stringify(v, null, 2))}</pre>`;
}

export function errorCard(msg, hint = '') {
  return `<div class="card"><div class="alert danger">${esc(msg)}</div>${hint ? `<div class="hint" style="margin-top:8px">${hint}</div>` : ''}</div>`;
}

export async function logout() {
  try { await api.post('/api/panel/logout'); } catch (e) { /* ignore */ }
  location.reload();
}
