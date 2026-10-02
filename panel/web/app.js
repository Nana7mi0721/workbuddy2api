// 面板外壳：登录门、侧边栏、路由、自动刷新调度
import { esc, api } from './util.js';
import { toastErr, toast, delegate, logout } from './ui.js';
import { pages } from './pages.js';

const REFRESH_STEPS = [0, 3000, 5000, 10000, 30000];

const app = {
  info: null,
  page: null,
  ctx: null,
  autoMs: 5000,
};

function routeId() {
  const h = (location.hash || '').replace(/^#\/?/, '').split('?')[0];
  return pages.some((p) => p.id === h) ? h : 'overview';
}

function ctxFor(pageId) {
  const timers = [];
  const reloads = [];
  const cleanups = [];
  let stale = false;
  const ctx = {
    info: app.info,
    stale: () => stale,
    autoMs: () => app.autoMs,
    every(fn, ms) {
      if (!ms) return;
      timers.push(setInterval(() => { if (!document.hidden) fn(); }, ms));
    },
    onReload(fn) { reloads.push(fn); },
    onCleanup(fn) { cleanups.push(fn); },
    reload() { reloads.forEach((f) => f()); },
    cycleAuto() {
      const i = REFRESH_STEPS.indexOf(app.autoMs);
      app.autoMs = REFRESH_STEPS[(i + 1) % REFRESH_STEPS.length];
      localStorage.setItem('wb-auto', String(app.autoMs));
      toast(app.autoMs ? `自动刷新：${app.autoMs / 1000} 秒` : '自动刷新已关闭');
      navigate();
    },
    dispose() {
      stale = true;
      timers.forEach(clearInterval);
      cleanups.forEach((f) => { try { f(); } catch (e) { /* ignore */ } });
    },
  };
  return ctx;
}

function shell() {
  const nav = pages.map((p) => `<a href="#/${p.id}" data-nav="${p.id}"><span class="ico">${p.icon}</span>${esc(p.label)}</a>`).join('');
  const theme = localStorage.getItem('wb-theme') || 'light';
  document.documentElement.dataset.theme = theme;
  const html = `
  <div class="app">
    <div class="side-mask" data-act="menu"></div>
    <aside class="sidebar">
      <div class="brand">
        <div class="brand-mark">W</div>
        <div class="brand-text"><b>workbuddy2api</b><span>管理面板 v${esc(app.info.version)}</span></div>
      </div>
      <nav class="nav">${nav}</nav>
      <div class="side-foot">
        <div>网关 <code>${esc(app.info.gateway)}</code></div>
        <div class="row">${app.info.admin_enabled ? '<span class="badge ok"><i class="dot"></i>admin</span>' : '<span class="badge warn"><i class="dot"></i>admin 关</span>'}
          ${app.info.metrics_on ? '<span class="badge ok"><i class="dot"></i>metrics</span>' : '<span class="badge">metrics 关</span>'}</div>
        <div class="row" style="margin-top:10px">
          <button class="btn ghost sm" data-act="theme">切换主题</button>
          <button class="btn ghost sm" data-act="reloadInfo">重载</button>
          ${app.info.auth_required ? '<button class="btn ghost sm" data-act="logout">退出</button>' : ''}
        </div>
      </div>
    </aside>
    <main class="main">
      <button class="btn icon menu-btn" data-act="menu" aria-label="菜单">☰</button>
      <div class="page" id="view"></div>
    </main>
  </div>`;
  const root = document.getElementById('app');
  root.innerHTML = html;
  const closeSide = () => root.querySelector('.app')?.classList.remove('side-open');
  root.querySelector('.nav').addEventListener('click', (e) => {
    const a = e.target.closest('[data-nav]');
    if (a) { e.preventDefault(); closeSide(); location.hash = '#/' + a.dataset.nav; }
  });
  delegate(root, {
    menu: () => root.querySelector('.app')?.classList.toggle('side-open'),
    theme: () => {
      const next = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
      document.documentElement.dataset.theme = next;
      localStorage.setItem('wb-theme', next);
    },
    reloadInfo: () => boot(true),
    logout: () => logout(),
  });
  const view = root.querySelector('#view');
  view.addEventListener('wb:reload', () => { if (app.ctx) app.ctx.reload(); });
  return view;
}

function mount() {
  const id = routeId();
  const page = pages.find((p) => p.id === id);
  if (app.ctx) app.ctx.dispose();
  const view = document.getElementById('view');
  view.scrollTop = 0;
  document.querySelectorAll('[data-nav]').forEach((a) => a.classList.toggle('active', a.dataset.nav === id));
  document.title = `${page.label} · workbuddy2api 面板`;
  app.page = page;
  app.ctx = ctxFor(id);
  try {
    page.mount(view, app.ctx);
  } catch (e) {
    view.innerHTML = `<div class="card"><div class="alert danger">页面渲染失败：${esc(e.message)}</div></div>`;
  }
}

const navigate = () => { if (document.getElementById('view')) mount(); };

function loginScreen(msg = '') {
  document.getElementById('app').innerHTML = `
  <div style="height:100%;display:grid;place-items:center">
    <div class="card" style="width:min(420px,92vw)">
      <div class="row" style="margin-bottom:14px"><div class="brand-mark">W</div><div class="brand-text"><b>workbuddy2api 面板</b><span>请先登录</span></div></div>
      ${msg ? `<div class="alert danger" style="margin-bottom:12px">${esc(msg)}</div>` : ''}
      <label class="field"><span>访问密码</span><input type="password" id="pw" autocomplete="current-password"></label>
      <button class="btn primary" id="go" style="width:100%">登录</button>
      <div class="hint" style="margin-top:10px">密码在面板 <code>panel.json</code> 的 <code>auth.password</code> 中配置；留空表示不启用登录。</div>
    </div>
  </div>`;
  const go = async () => {
    const pw = document.getElementById('pw').value;
    const btn = document.getElementById('go');
    btn.disabled = true;
    try {
      await api.post('/api/panel/login', { password: pw });
      await boot();
    } catch (e) {
      loginScreen(e.message);
    }
  };
  document.getElementById('go').addEventListener('click', go);
  document.getElementById('pw').addEventListener('keydown', (e) => { if (e.key === 'Enter') go(); });
  document.getElementById('pw').focus();
}

async function boot(silent) {
  let info;
  try {
    info = await api.get('/api/panel/info');
  } catch (e) {
    document.getElementById('app').innerHTML = `<div style="padding:60px"><div class="alert danger">无法连接面板后端：${esc(e.message)}</div></div>`;
    return;
  }
  app.info = info;
  if (info.auth_required && !info.authed) { loginScreen(); return; }
  if (silent) {
    // 仅刷新配置元信息，不重建 DOM
    if (app.ctx) { app.ctx.info = app.info; }
    toast('面板信息已重新读取');
    return;
  }
  const view = shell();
  app.autoMs = Number(localStorage.getItem('wb-auto') ?? 5000);
  if (!REFRESH_STEPS.includes(app.autoMs)) app.autoMs = 5000;
  mount();
  window.addEventListener('hashchange', navigate);
  document.addEventListener('visibilitychange', () => { if (!document.hidden && app.ctx) app.ctx.reload(); });
  return view;
}

boot().catch((e) => {
  document.getElementById('app').innerHTML = `<div style="padding:60px"><div class="alert danger">面板启动失败：${esc(e.message)}</div></div>`;
});
