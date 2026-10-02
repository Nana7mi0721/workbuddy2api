// 各功能页：概览 / API 配置 / 账号 / 统计 / 日志 / 配置 / 服务
import { esc, api, all, isErr, num, flt, pct, bytes, dur, ts, tsShort, ago, uid8, inlineKV, objKV } from './util.js';
import { toast, toastErr, toastWarn, busy, openModal, confirmModal, promptModal, delegate, copy, badge, dotBadge, kpi, table, statusBadge, pageHead, jsonBlock, errorCard } from './ui.js';

const realmBadge = (r) => (r === 'global' ? badge('Global', 'purple') : r === 'cn' ? badge('CN', 'info') : badge(r || '未知'));
const stateBadge = (a) => {
  if (a.manual_disabled) return dotBadge('手动禁用', 'danger');
  if (a.disabled) return dotBadge('自动禁用', 'danger');
  if (a.cooling) return dotBadge('冷却中', 'warn');
  if (a.in_pool) return dotBadge('可服务', 'ok');
  if (a.file_paused) return dotBadge('已暂停', '');
  return dotBadge('未加载', '');
};

// ============================================================ 概览

export const overview = {
  id: 'overview', label: '概览', icon: '◎',
  mount(root, ctx) {
    let lastData = null;
    const load = async () => {
      const d = await all({
        ov: api.get('/api/overview'),
        tasks: api.get('/api/tasks'),
        logs: api.get('/api/logs?limit=120&kind=chat'),
      });
      if (ctx.stale()) return;
      lastData = d;
      root.innerHTML = renderOverview(d, ctx);
      // rAF：innerHTML 刚插入还没有布局，clientWidth 为 0，canvas 必须等布局后再画。
      requestAnimationFrame(() => drawOverviewChart(root, d));
    };
    const onResize = () => { if (lastData && !ctx.stale()) requestAnimationFrame(() => drawOverviewChart(root, lastData)); };
    window.addEventListener('resize', onResize);
    ctx.onCleanup(() => window.removeEventListener('resize', onResize));
    root.innerHTML = `<div class="empty">正在读取网关状态…</div>`;
    load().catch((e) => { console.error(e); if (!ctx.stale()) root.innerHTML = errorCard('加载失败：' + e.message); });
    ctx.every(load, Math.max(3000, ctx.info.ui.refresh_ms || 5000));
    ctx.onReload(load);
    delegate(root, {
      reload: () => root.dispatchEvent(new CustomEvent('wb:reload')),
      toggleRef: () => ctx.cycleAuto(),
      checkin: async (el) => {
        const okc = await confirmModal('一键签到', '将对全部账号执行一次签到 + 余额刷新（每号 2~3 次上游调用），确认执行？', { okLabel: '签到' });
        if (!okc) return;
        busy(el);
        try {
          const r = await api.post('/api/checkin-proxy');
          if (r.report) {
            const rp = r.report;
            toast(`签到完成：成功 ${rp.ok ?? 0} · 已签 ${rp.already ?? 0} · 失败 ${rp.fail ?? 0} · 跳过 ${rp.skipped ?? 0}`, 'ok', 5200);
          } else {
            toast(r.message || '签到已执行');
          }
          setTimeout(() => root.dispatchEvent(new CustomEvent('wb:reload')), 800);
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
      restartGw: async (el) => {
        const okc = await confirmModal('重启网关', '重启会造成 1~5 秒的请求中断，确认继续？', { okLabel: '重启' });
        if (!okc) return;
        busy(el);
        try {
          const r = await api.post('/api/service/restart');
          toast(r.message || '重启完成');
          setTimeout(() => root.dispatchEvent(new CustomEvent('wb:reload')), 1500);
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
      runTask: async (el) => {
        busy(el);
        try {
          const r = await api.post(`/api/tasks/${encodeURIComponent(el.dataset.name)}/run`);
          toast(r.note || (el.dataset.name + ' 已触发'));
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
      startSvc: async (el) => {
        busy(el);
        try {
          const r = await api.post('/api/service/start');
          toast(r.message || '已启动');
          setTimeout(() => root.dispatchEvent(new CustomEvent('wb:reload')), 1200);
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
    });
  },
};

function renderOverview(d, ctx) {
  const ov = isErr(d.ov) ? null : d.ov;
  const svc = ov && ov.service ? ov.service : null;
  const health = ov && ov.health ? ov.health : null;
  const st = ov && ov.status ? ov.status : null;
  const stats = ov && ov.stats ? ov.stats : null;
  const tasks = isErr(d.tasks) ? null : d.tasks;
  const logs = isErr(d.logs) ? null : d.logs;

  const parts = [];
  const upSec = stats && stats.uptime_sec
    ? stats.uptime_sec
    : (ov && ov.panel && ov.panel.started_at ? (Date.now() - new Date(ov.panel.started_at).getTime()) / 1000 : 0);
  parts.push(pageHead('概览',
    `网关 <code>${esc(ctx.info.gateway)}</code> · 面板 v${esc(ctx.info.version)} · ${stats && stats.uptime_sec ? '网关已运行' : '面板已运行'} ${esc(dur(upSec))}`,
    `<a class="btn primary" href="#/access">API 配置</a>
     <button class="btn" data-act="checkin">一键签到</button>
     <button class="btn" data-act="restartGw">重启网关</button>
     <button class="btn" data-act="reload">刷新</button>
     <button class="btn" data-act="toggleRef">自动刷新：${ctx.autoMs() ? ctx.autoMs() / 1000 + 's' : '关'}</button>`));

  // 健康横幅
  if (isErr(d.ov)) {
    parts.push(`<div class="alert danger">无法连接网关：${esc(d.ov.__error)}　<button class="btn sm" data-act="startSvc">启动网关</button> <a class="btn sm" href="#/service">服务页</a></div>`);
  } else if (ov.health_error) {
    parts.push(`<div class="alert danger">健康检查失败：${esc(ov.health_error)}</div>`);
  } else if (health) {
    // /api/overview 里的 health 是上游 /healthz 原文；/api/health 则包了一层 body。
    const body = health.body || health;
    const okAll = health.healthy !== undefined ? health.healthy : body.healthy > 0;
    const rs = body.realm_servable || {};
    parts.push(`<div class="alert ${okAll ? 'ok' : 'danger'}">
      ${okAll ? '网关运行正常' : '网关可访问但账号不可服务'}：健康账号 <b>${esc(num(body.healthy))}/${esc(num(body.total))}</b>
      · CN ${rs.cn ? '✓' : '✗'} · Global ${rs.global ? '✓' : '✗'}
      · 服务名 <code>${esc(body.service || 'workbuddy2api')}</code>${health.elapsed_ms != null ? ` · 响应 ${esc(num(health.elapsed_ms))} ms` : ''}
    </div>`);
  }
  if (st && st.in_flight_full) parts.push(`<div class="alert warn" style="margin-top:10px">在途请求已达上限（in_flight_full=${esc(st.in_flight_full)}），新请求会排队或返回 busy。</div>`);

  // KPI
  const credits = st && st.accounts ? st.accounts.reduce((s, a) => s + (a.credits || 0), 0) : 0;
  const inFlight = st && st.accounts ? st.accounts.reduce((s, a) => s + (a.in_flight || 0), 0) : 0;
  const budget = st && st.daily_budget;
  const rt = (st && st.realm_totals) || {};
  const rtText = Object.entries(rt)
    .filter(([, c]) => c && typeof c === 'object')
    .map(([r, c]) => `${r.toUpperCase()} ${num(c.healthy)}/${num(c.total)}`)
    .join(' · ');
  parts.push(`<div class="grid kpis section">
    ${kpi('可服务账号', st ? `${num(st.healthy)}<span class="hint" style="font-size:14px"> / ${num(st.total)}</span>` : '—', st ? `冷却 ${num(st.cooling)} · 禁用 ${num(st.disabled)}` : '')}
    ${kpi('在途请求', st ? num(inFlight) : '—', st && st.in_flight_full ? '已达上限' : '未占满')}
    ${kpi('积分合计', st ? num(credits) : '—', rtText)}
    ${kpi('今日预算', budget ? (Number(budget.limit) > 0 ? `${num(budget.used)} / ${num(budget.limit)}` : '未启用') : '—', budget ? `拒绝 ${num(budget.rejected)} · ${esc(budget.day || '')}` : '')}
    ${kpi('累计请求', stats && stats.total ? num(stats.total.requests) : '—', stats && stats.total ? `成功率 ${pct(stats.total.requests ? stats.total.success / stats.total.requests : 0)}` : '')}
    ${kpi('平均 TTFB', stats && stats.total && stats.total.avg_ttfb_ms ? flt(stats.total.avg_ttfb_ms, 0) + ' ms' : '—', stats && stats.total ? `${flt(stats.total.tokens_per_sec, 1)} tok/s` : '')}
  </div>`);

  // 任务
  if (tasks && tasks.tasks) {
    parts.push(`<div class="section"><div class="section-title">定时任务 ${tasks.admin_enabled ? '' : '<span class="badge warn">需开启 admin.enabled 才能手动触发</span>'}</div><div class="grid cards">${tasks.tasks.map(taskCard).join('')}</div></div>`);
  }

  // 最近请求趋势（取本轮拉到的 chat 行画迷你趋势图，无第三方依赖）
  if (logs && logs.entries && logs.entries.length >= 2) {
    parts.push(`<div class="section"><div class="section-title">最近请求趋势 <span class="hint">（最近 ${logs.entries.length} 次请求 · TTFB 与生成速度）</span></div>
      <div class="card chart-card"><canvas id="ov-chart" height="150"></canvas>
        <div class="chart-legend">
          <span><i class="dot" style="color:var(--info)"></i>TTFB ms（左轴）</span>
          <span><i class="dot" style="color:var(--primary)"></i>tok/s（右轴）</span>
          <span class="hint" id="ov-chart-range"></span>
        </div>
      </div></div>`);
  }

  // 最近请求
  if (logs && logs.entries) {
    const rows = logs.entries.slice(-8).reverse().map((e) => [
      `<span class="mono">${esc(e.time || '')}</span>`,
      esc(e.model || '—'),
      badge(e.mode || '', e.mode === 'stream' ? 'info' : ''),
      statusBadge(e.status),
      esc(e.user || '—') + (e.uid8 ? ` <span class="mono hint">${esc(e.uid8)}</span>` : ''),
      num(e.ttfb_ms) + ' ms',
      num(e.tokens),
      flt(e.tok_per_sec, 1),
      flt(e.total_s, 2) + ' s',
    ]);
    parts.push(`<div class="section"><div class="section-title">最近请求 <a class="hint" href="#/logs" style="margin-left:6px">查看全部 →</a></div>
      ${table([{ label: '时间' }, { label: '模型' }, { label: '模式' }, { label: '状态' }, { label: '账号' }, { label: 'TTFB', cls: 'num' }, { label: 'tokens', cls: 'num' }, { label: 'tok/s', cls: 'num' }, { label: '耗时', cls: 'num' }], rows, '还没有请求记录')}</div>`);
  }

  return parts.join('');
}

// drawOverviewChart 把本轮拉到的 chat 行画成双序列迷你趋势图（左轴 TTFB，右轴 tok/s）。
// 纯 canvas 无依赖；文件序（line 号）即时间序。dpr 感知 + 主题色取自 CSS 变量。
function drawOverviewChart(root, d) {
  const cv = root.querySelector('#ov-chart');
  if (!cv) return;
  const logs = d && !isErr(d.logs) ? d.logs : null;
  const entries = (logs && logs.entries ? logs.entries : []).slice().sort((a, b) => (a.line || 0) - (b.line || 0));
  const pts = entries.filter((e) => Number(e.ttfb_ms) > 0 && Number(e.tok_per_sec) > 0);
  if (pts.length < 2 || !cv.clientWidth) return;
  const css = getComputedStyle(document.documentElement);
  const cInfo = (css.getPropertyValue('--info') || '#6aa9ff').trim();
  const cPrim = (css.getPropertyValue('--primary') || '#4dbf9b').trim();
  const cGrid = (css.getPropertyValue('--border') || 'rgba(127,127,127,.25)').trim();
  const cMuted = (css.getPropertyValue('--muted') || '#8792a4').trim();
  const dpr = window.devicePixelRatio || 1;
  const w = cv.clientWidth, h = 150;
  cv.width = Math.round(w * dpr); cv.height = Math.round(h * dpr);
  const g = cv.getContext('2d');
  g.scale(dpr, dpr);
  g.clearRect(0, 0, w, h);
  const pad = { l: 48, r: 56, t: 12, b: 18 };
  const iw = w - pad.l - pad.r, ih = h - pad.t - pad.b;
  const X = (i) => pad.l + (iw * i) / (pts.length - 1);
  const series = [
    { key: 'ttfb_ms', color: cInfo, axis: 'l', fmt: (v) => Math.round(v) + 'ms' },
    { key: 'tok_per_sec', color: cPrim, axis: 'r', fmt: (v) => flt(v, 0) },
  ];
  g.strokeStyle = cGrid; g.lineWidth = 1;
  for (let i = 0; i <= 2; i++) {
    const y = pad.t + (ih * i) / 2;
    g.beginPath(); g.moveTo(pad.l, y); g.lineTo(pad.l + iw, y); g.stroke();
  }
  g.font = '10.5px ' + (css.getPropertyValue('--mono') || 'monospace');
  const ranges = series.map((s) => {
    const vals = pts.map((e) => Number(e[s.key]));
    let min = Math.min(...vals), max = Math.max(...vals);
    const pv = (max - min) * 0.12 || Math.abs(max) * 0.12 || 1;
    min -= pv; max += pv;
    if (min < 0) min = 0; // TTFB/tok/s 都非负，padding 别越过 0（否则轴标签出现 -1）
    return { min, max };
  });
  series.forEach((s, si) => {
    const Y = (v) => pad.t + ih - ((v - ranges[si].min) / (ranges[si].max - ranges[si].min)) * ih;
    g.strokeStyle = s.color; g.lineWidth = 1.8;
    g.beginPath();
    pts.forEach((e, i) => { const x = X(i), y = Y(Number(e[s.key])); i ? g.lineTo(x, y) : g.moveTo(x, y); });
    g.stroke();
    const lx = X(pts.length - 1), ly = Y(Number(pts[pts.length - 1][s.key]));
    g.fillStyle = s.color;
    g.beginPath(); g.arc(lx, ly, 3, 0, Math.PI * 2); g.fill();
    g.textAlign = s.axis === 'l' ? 'right' : 'left';
    const ax = s.axis === 'l' ? pad.l - 6 : pad.l + iw + 6;
    g.fillStyle = cMuted;
    g.fillText(s.fmt(ranges[si].max), ax, pad.t + 8);
    g.fillText(s.fmt(ranges[si].min), ax, pad.t + ih);
  });
  const rangeEl = root.querySelector('#ov-chart-range');
  if (rangeEl) rangeEl.textContent = `${pts[0].time || ''} → ${pts[pts.length - 1].time || ''}`;
}

// hl 转义并高亮：q（不区分大小写）出现处包 <mark>，其余照常转义。
function hl(text, q) {
  const s = String(text ?? '');
  if (!q) return esc(s);
  const lower = s.toLowerCase(), needle = String(q).toLowerCase();
  let out = '', i = 0;
  for (;;) {
    const j = lower.indexOf(needle, i);
    if (j < 0) { out += esc(s.slice(i)); break; }
    out += esc(s.slice(i, j)) + '<mark>' + esc(s.slice(j, j + needle.length)) + '</mark>';
    i = j + needle.length;
  }
  return out;
}

function taskCard(t) {
  const stateMap = { ok: ['上次成功', 'ok'], partial: ['部分失败', 'warn'], all_failed: ['全部失败', 'danger'] };
  const s = stateMap[t.last_state] || ['未运行过', ''];
  return `<div class="card">
    <div class="row"><h3>${esc(t.label)}</h3><div class="spacer"></div>${t.enabled ? badge('已启用', 'ok') : badge('已停用', '')} ${badge(s[0], s[1])}</div>
    <div class="sub" style="margin-bottom:8px">${esc(t.hint || '')}</div>
    <div class="kv">
      <dt>上次运行</dt><dd>${esc(t.last_run ? tsShort(t.last_run) : '—')}${t.trigger ? ` · ${esc({ schedule: '定时', retry: '重试', manual: '手动' }[t.trigger] || t.trigger)}` : ''}</dd>
      <dt>结果</dt><dd>${t.total != null ? `成功 ${num(t.ok)} · 幂等 ${num(t.already)} · 失败 ${num(t.fail)} / 共 ${num(t.total)}` : '—'}${t.elapsed ? ` · 耗时 ${esc(t.elapsed)}` : ''}</dd>
      ${t.note ? `<dt>备注</dt><dd>${esc(t.note)}</dd>` : ''}
    </div>
    <div class="row" style="margin-top:10px"><div class="spacer"></div><button class="btn sm" data-act="runTask" data-name="${esc(t.name)}">立即执行</button></div>
  </div>`;
}

// ============================================================ 账号

export const accounts = {
  id: 'accounts', label: '账号', icon: '◍',
  mount(root, ctx) {
    const load = async () => {
      const d = await all({ acc: api.get('/api/accounts'), tasks: api.get('/api/tasks') });
      if (ctx.stale()) return;
      if (isErr(d.acc)) { root.innerHTML = pageHead('账号', '') + errorCard('读取失败：' + d.acc.__error); return; }
      root.innerHTML = renderAccounts(d.acc, d, ctx);
    };
    root.innerHTML = `<div class="empty">正在读取账号…</div>`;
    load().catch((e) => { console.error(e); if (!ctx.stale()) root.innerHTML = errorCard('加载失败：' + e.message); });
    ctx.every(load, Math.max(4000, ctx.info.ui.refresh_ms || 5000));
    ctx.onReload(load);

    delegate(root, {
      reload: () => root.dispatchEvent(new CustomEvent('wb:reload')),
      addAccount: () => addAccountFlow(root, ctx),
      copyUid: (el) => copy(el.dataset.uid, 'uid 已复制'),
      act: async (el) => {
        const uid = el.dataset.uid, action = el.dataset.action;
        const names = { disable: '手动禁用', enable: '清除手动禁用', revive: '清除自动禁用/熔断', pause: '暂停（移出 auths）', resume: '恢复（放回 auths）' };
        if (action === 'pause' || action === 'disable') {
          const okc = await confirmModal('确认操作', `账号 <code>${esc(uid8(uid))}</code> 即将执行「${names[action] || action}」。`, { okLabel: '执行' });
          if (!okc) return;
        }
        busy(el);
        try {
          const r = await api.post(`/api/accounts/${encodeURIComponent(uid)}/${action}`);
          toast(r.note || names[action] + ' 成功');
          setTimeout(() => root.dispatchEvent(new CustomEvent('wb:reload')), 600);
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
      del: async (el) => {
        const uid = el.dataset.uid;
        const okc = await promptModal('删除账号文件', `将从 <code>auths-paused\\</code> 中永久删除账号文件（uid 前 8 位）：`, uid8(uid), { placeholder: uid8(uid) });
        if (!okc) return;
        busy(el);
        try {
          const r = await api.del(`/api/accounts/${encodeURIComponent(uid)}`);
          toast(r.note || '已删除');
          setTimeout(() => root.dispatchEvent(new CustomEvent('wb:reload')), 400);
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
    });
  },
};

function renderAccounts(d, dd, ctx) {
  const list = d.accounts || [];
  const s = d.summary || {};
  const adminOn = ctx.info.admin_enabled;
  const parts = [];
  parts.push(pageHead('账号', `${list.length} 个账号 · 配置文件 ${num(s.active_files)} 个（暂停 ${num(s.paused_files)} 个）· state.json 更新于 ${esc(s.state_updated ? ago(s.state_updated) : '—')}`,
    `<button class="btn" data-act="reload">刷新</button><button class="btn primary" data-act="addAccount">+ 添加账号</button>`));

  if (!adminOn) parts.push(`<div class="alert warn" style="margin-bottom:14px">网关 <code>config.json</code> 的 <code>admin.enabled=false</code>，因此「禁用 / 启用 / 复活」与手动触发任务不可用（<a href="#/config">去配置页一键开启</a>，开启后需重启网关）。暂停 / 恢复 / 删除账号走文件系统，不受影响。</div>`);
  if (s.error) parts.push(`<div class="alert danger" style="margin-bottom:14px">${esc(s.error)}</div>`);

  const chips = [];
  const pc = s.pool_counts || {};
  const usable = pc.total != null ? (pc.healthy || 0) : list.filter((a) => a.in_pool && !a.disabled && !a.cooling).length;
  chips.push(`<span class="badge ok">可服务 ${num(usable)}${pc.total != null ? ' / ' + num(pc.total) : ''}</span>`);
  chips.push(`<span class="badge warn">冷却 ${num(pc.cooling != null ? pc.cooling : list.filter((a) => a.cooling).length)}</span>`);
  chips.push(`<span class="badge danger">禁用 ${num(pc.disabled != null ? pc.disabled : list.filter((a) => a.disabled || a.manual_disabled).length)}</span>`);
  chips.push(`<span class="badge">暂停文件 ${num(s.paused_files || 0)}</span>`);
  const rt = s.realm_totals || {};
  for (const [realm, c] of Object.entries(rt)) {
    if (!c || typeof c !== 'object') continue;
    chips.push(`<span class="badge ${realm === 'global' ? 'purple' : 'info'}">${esc(realm.toUpperCase())} 健康 ${num(c.healthy)}/${num(c.total)}${c.cooling ? ' · 冷却 ' + num(c.cooling) : ''}${c.disabled ? ' · 禁用 ' + num(c.disabled) : ''}</span>`);
  }
  if (s.sticky != null && s.sticky !== '') chips.push(`<span class="badge">粘性会话 ${esc(typeof s.sticky === 'object' ? JSON.stringify(s.sticky) : s.sticky)}</span>`);
  if (s.in_flight_full) chips.push(`<span class="badge danger">在途已占满</span>`);
  parts.push(`<div class="row wrap" style="margin-bottom:14px">${chips.join('')}</div>`);

  if (!list.length) parts.push(`<div class="card"><div class="empty">还没有任何账号。点右上角「+ 添加账号」用官方授权码流程登录一个 WorkBuddy 账号。</div></div>`);

  parts.push(`<div class="grid cards">${list.map((a) => accountCard(a, adminOn)).join('')}</div>`);
  return parts.join('');
}

function accountCard(a, adminOn) {
  const now = Date.now() / 1000;
  const costs = a.model_costs && typeof a.model_costs === 'object' ? objKV(a.model_costs) : [];
  const rl = Array.isArray(a.rate_limited_models) ? a.rate_limited_models : [];
  const expiring = a.credits_expiring && (Array.isArray(a.credits_expiring) ? a.credits_expiring.length : Object.keys(a.credits_expiring || {}).length);
  const tokenInfo = a.token_expires_at ? (a.token_expired ? badge('token 已过期', 'danger') : badge('token 有效至 ' + tsShort(a.token_expires_at * 1000), a.token_expires_at - now < 86400 ? 'warn' : '')) : badge('无本地文件', '');
  return `<div class="card acct-card">
    <div class="acct-top">
      <div class="brand-mark" style="background:linear-gradient(140deg,#4dbf9b,#2f8f74)">${esc((a.nickname || '?').slice(0, 1))}</div>
      <div style="flex:1;min-width:0">
        <div class="row"><div class="acct-name">${esc(a.nickname || '(未命名账号)')}</div></div>
        <div class="acct-meta">${esc(uid8(a.uid))} · ${esc(a.uid)}</div>
      </div>
      <div class="row wrap" style="justify-content:flex-end">${realmBadge(a.realm)} ${stateBadge(a)}</div>
    </div>
    <div class="acct-stats">
      <div><span>积分</span><b>${num(a.credits)}</b></div>
      <div><span>成功</span><b>${num(a.success_count)}</b></div>
      <div><span>错误</span><b>${num(a.err_total)}</b></div>
      <div><span>连续失败</span><b>${num(a.consecutive_fails)}</b></div>
    </div>
    <div class="row wrap">${tokenInfo}${a.in_flight ? badge('在途 ' + a.in_flight, 'info') : ''}${a.file_paused ? badge('文件在 auths-paused', '') : ''}${!a.in_pool && !a.file_paused ? badge('网关未加载', '') : ''}${a.broken ? badge('文件解析失败', 'danger') : ''}</div>
    <details class="inline"><summary>详细信息</summary>
      <div class="kv" style="margin-top:8px">
        <dt>冷却</dt><dd>${a.cooling ? `${esc(a.cool_kind || '')} · 剩 ${esc(dur(a.cool_remaining_sec))}${a.reason ? ' · ' + esc(a.reason) : ''}` : '否'}</dd>
        <dt>禁用原因</dt><dd>${esc(a.disabled_reason || a.manual_reason || '—')}</dd>
        <dt>最近成功</dt><dd>${esc(a.last_success ? ts(a.last_success) : '—')}</dd>
        <dt>最近错误</dt><dd>${esc(a.last_err || '—')}</dd>
        <dt>熔断</dt><dd>${a.breaker_until || a.breaker_fails ? `${num(a.breaker_fails)} 次 · 至 ${esc(a.breaker_until || '—')}` : '—'}</dd>
        <dt>降级至</dt><dd>${esc(a.degrade_until || '—')}</dd>
        <dt>state.json</dt><dd>积分 ${num(a.state_credits)} · 最近成功 ${esc(a.last_state_update || '—')}</dd>
        <dt>账号文件</dt><dd>${a.file_name ? `<code>${esc(a.file_name)}</code>（${a.file_active ? 'auths' : 'auths-paused'}，修改于 ${esc(tsShort(a.file_modtime * 1000))}）` : '—'}</dd>
        <dt>过期积分</dt><dd>${expiring ? `<code>${esc(JSON.stringify(a.credits_expiring))}</code>` : '—'}</dd>
      </div>
      ${rl.length ? `<div style="margin-top:8px"><div class="hint">限流模型</div>${rl.map((r) => `<div class="mono" style="font-size:12px">${esc(r.model)} → ${esc(r.reason || '')} 至 ${esc(r.until || r.reset_at || '')}</div>`).join('')}</div>` : ''}
      ${costs.length ? `<div style="margin-top:8px"><div class="hint">模型成本（cost_per_1k）</div>
        ${table([{ label: '模型' }, { label: '倍率', cls: 'num' }, { label: '采集数', cls: 'num' }, { label: '最近' }],
          costs.map(([m, v]) => [esc(m), flt(v && v.cost_per_1k, 4), num(v && v.samples), esc((v && v.last_seen) || '—')]))}</div>` : ''}
    </details>
    <div class="acct-actions">
      ${adminOn ? (a.manual_disabled
        ? `<button class="btn sm" data-act="act" data-action="enable" data-uid="${esc(a.uid)}">清除手动禁用</button>`
        : `<button class="btn sm danger" data-act="act" data-action="disable" data-uid="${esc(a.uid)}">手动禁用</button>`)
        : `<button class="btn sm" disabled title="需 admin.enabled">手动禁用</button>`}
      ${a.disabled || a.breaker_until || a.degrade_until ? `<button class="btn sm" data-act="act" data-action="revive" data-uid="${esc(a.uid)}" ${adminOn ? '' : 'disabled'}>复活</button>` : ''}
      ${a.file_active ? `<button class="btn sm" data-act="act" data-action="pause" data-uid="${esc(a.uid)}">暂停</button>` : ''}
      ${a.file_paused ? `<button class="btn sm" data-act="act" data-action="resume" data-uid="${esc(a.uid)}">恢复</button>` : ''}
      ${a.file_paused ? `<button class="btn sm danger" data-act="del" data-uid="${esc(a.uid)}">删除文件</button>` : ''}
      <div class="spacer"></div>
      <button class="btn sm ghost" data-act="copyUid" data-uid="${esc(a.uid)}">复制 uid</button>
    </div>
  </div>`;
}

function addAccountFlow(root, ctx) {
  const m = openModal({
    title: '添加账号',
    width: '640px',
    body: `
      <ol class="steps">
        <li>选择账号区域（CN = copilot.tencent.com，Global = workbuddy.ai）。</li>
        <li>点「获取授权链接」，浏览器打开该链接并完成登录 / 授权。</li>
        <li>回到本页点「我已授权，开始轮询」，成功后账号文件会自动写入 <code>${esc(ctx.info.base_dir)}\\auths\\</code>。</li>
      </ol>
      <div class="form-row" style="align-items:flex-end">
        <label class="field"><span>账号区域</span>
          <select id="la-realm"><option value="cn">CN · 腾讯 Copilot</option><option value="global">Global · workbuddy.ai</option></select>
        </label>
        <button class="btn primary" id="la-start" style="margin-bottom:12px">获取授权链接</button>
      </div>
      <div id="la-step2" style="display:none">
        <label class="field"><span>授权链接</span>
          <div class="link-box"><input type="text" id="la-url" readonly><button class="btn" id="la-copy">复制</button><button class="btn" id="la-open">打开</button></div>
          <em>若浏览器无法直接打开，请手动复制到浏览器。</em>
        </label>
        <div class="row"><button class="btn primary" id="la-poll">我已授权，开始轮询</button><span class="hint" id="la-status">等待操作…</span></div>
      </div>
      <div id="la-result" style="margin-top:12px"></div>`,
    footer: `<button class="btn" data-close>关闭</button>`,
  });

  const startBtn = m.el.querySelector('#la-start');
  const pollBtn = m.el.querySelector('#la-poll');
  const statusEl = m.el.querySelector('#la-status');
  const resultEl = m.el.querySelector('#la-result');
  let realm = 'cn';

  m.el.querySelector('#la-realm').addEventListener('change', (e) => { realm = e.target.value; });

  startBtn.addEventListener('click', async () => {
    busy(startBtn);
    resultEl.innerHTML = '';
    try {
      const r = await api.post('/api/accounts/login/start', { realm });
      m.el.querySelector('#la-step2').style.display = '';
      m.el.querySelector('#la-url').value = r.url || '(未获取到链接)';
      statusEl.textContent = '请在新标签页完成授权，然后点右侧按钮';
      m.el.querySelector('#la-open').onclick = () => window.open(r.url, '_blank', 'noopener');
      m.el.querySelector('#la-copy').onclick = () => copy(r.url, '授权链接已复制');
    } catch (e) {
      resultEl.innerHTML = `<div class="alert danger">${esc(e.message)}</div>`;
    }
    busy(startBtn, false);
  });

  pollBtn.addEventListener('click', async () => {
    busy(pollBtn);
    statusEl.textContent = '轮询中…';
    try {
      const r = await api.post('/api/accounts/login/poll', { realm, save: true });
      statusEl.textContent = '成功';
      resultEl.innerHTML = `<div class="alert ok">账号 <b>${esc(r.nickname || '')}</b>（${esc(uid8(r.uid))}）已写入 ${esc(r.file || '')}，网关 5 秒内自动加载。</div>`;
      toast('账号添加成功：' + (r.nickname || uid8(r.uid)));
      setTimeout(() => root.dispatchEvent(new CustomEvent('wb:reload')), 800);
    } catch (e) {
      statusEl.textContent = '尚未授权完成';
      resultEl.innerHTML = `<div class="alert warn">${esc(e.message)}</div><div class="hint">如果已在浏览器完成授权，稍等几秒再点一次；若反复失败，请重新「获取授权链接」（state 可能已过期）。</div>`;
    }
    busy(pollBtn, false);
  });
}

// ============================================================ 统计

export const stats = {
  id: 'stats', label: '统计', icon: '◔',
  mount(root, ctx) {
    const load = async () => {
      const d = await all({ stats: api.get('/api/stats') });
      if (ctx.stale()) return;
      if (isErr(d.stats)) { root.innerHTML = pageHead('统计', '') + errorCard('读取失败：' + d.stats.__error); return; }
      root.innerHTML = renderStats(d.stats, ctx);
    };
    root.innerHTML = `<div class="empty">正在读取统计…</div>`;
    load().catch((e) => { console.error(e); if (!ctx.stale()) root.innerHTML = errorCard('加载失败：' + e.message); });
    ctx.every(load, Math.max(5000, ctx.info.ui.refresh_ms || 5000));

    delegate(root, {
      reload: () => root.dispatchEvent(new CustomEvent('wb:reload')),
      reset: async () => {
        const okc = await confirmModal('重置统计', '将清空网关内存中的请求统计（不影响账号积分与日志文件）。', { danger: true, okLabel: '重置' });
        if (!okc) return;
        try { await api.post('/api/stats/reset'); toast('统计已重置'); root.dispatchEvent(new CustomEvent('wb:reload')); } catch (e) { toastErr(e); }
      },
    });
  },
};

function renderStats(s, ctx) {
  if (s && s.enabled === false) return pageHead('统计', '') + errorCard('网关未启用统计（config.json 的 metrics.enabled=false）', '开启后需重启网关。');
  const t = s.total || {};
  const models = (s.models || []).filter((m) => m.model !== 'total');
  const parts = [];
  parts.push(pageHead('统计', `统计起点 ${esc(ts(s.since))} · 运行 ${esc(dur(s.uptime_sec))} · 生成于 ${esc(ts(s.now))}`,
    `<button class="btn" data-act="reload">刷新</button><button class="btn danger" data-act="reset">重置统计</button>`));
  parts.push(`<div class="grid kpis section">
    ${kpi('请求总数', num(t.requests), `成功 ${num(t.success)} · 失败 ${num(t.failed)}`)}
    ${kpi('成功率', t.requests ? pct(t.success / t.requests) : '—', `流式 ${num(t.streaming)}`)}
    ${kpi('平均 TTFB', t.avg_ttfb_ms ? flt(t.avg_ttfb_ms, 0) + ' ms' : '—', `延迟 ${t.avg_latency_ms ? flt(t.avg_latency_ms, 0) + ' ms' : '—'}`)}
    ${kpi('吞吐', t.tokens_per_sec ? flt(t.tokens_per_sec, 1) + ' tok/s' : '—', `tokens ${num(t.total_tokens)}`)}
    ${kpi('积分消耗', num(t.credit), t.requests ? `${flt(t.credit_per_req, 2)} / 请求` : '')}
    ${kpi('缓存命中', t.cache_hit_rate != null ? pct(t.cache_hit_rate) : '—', `命中 ${num(t.cache_hit_tokens)} · 未命中 ${num(t.cache_miss_tokens)}`)}
  </div>`);

  const rows = models.map((m) => {
    const nm = m.model && m.model.includes(':') ? m.model : m.model;
    return [
      `<span class="mono">${esc(nm)}</span>${m.credits ? ` <span class="badge">${esc(m.credits)}</span>` : ''}`,
      num(m.requests),
      num(m.success),
      num(m.failed) + (m.failed ? ' ' + badge('失败', 'danger') : ''),
      num(m.streaming),
      flt(m.avg_ttfb_ms, 0),
      flt(m.avg_latency_ms, 0),
      flt(m.tokens_per_sec, 1),
      num(m.prompt_tokens),
      num(m.completion_tokens),
      m.cache_hit_rate != null ? pct(m.cache_hit_rate) : '—',
      num(m.credit),
      flt(m.credit_per_req, 3),
      `<span class="hint">${esc(m.last_seen ? tsShort(m.last_seen) : '—')}</span>`,
    ];
  });
  if (t.model) {
    rows.unshift([
      `<b>总计</b>`, num(t.requests), num(t.success), num(t.failed), num(t.streaming),
      flt(t.avg_ttfb_ms, 0), flt(t.avg_latency_ms, 0), flt(t.tokens_per_sec, 1),
      num(t.prompt_tokens), num(t.completion_tokens), t.cache_hit_rate != null ? pct(t.cache_hit_rate) : '—',
      num(t.credit), flt(t.credit_per_req, 3), '',
    ]);
  }
  parts.push(`<div class="section"><div class="section-title">按模型</div>
    ${table([
      { label: '模型' }, { label: '请求', cls: 'num' }, { label: '成功', cls: 'num' }, { label: '失败', cls: 'num' },
      { label: '流式', cls: 'num' }, { label: 'TTFB ms', cls: 'num' }, { label: '延迟 ms', cls: 'num' }, { label: 'tok/s', cls: 'num' },
      { label: 'prompt', cls: 'num' }, { label: 'completion', cls: 'num' }, { label: '缓存命中', cls: 'num' },
      { label: '积分', cls: 'num' }, { label: '积分/请求', cls: 'num' }, { label: '最近' },
    ], rows, '还没有统计数据')}</div>`);
  return parts.join('');
}

// ============================================================ 日志

export const logs = {
  id: 'logs', label: '日志', icon: '≡',
  mount(root, ctx) {
    const st = {
      q: '', kind: 'all', level: 'all', uid: '', model: '', limit: 400,
      live: false, follow: true, entries: [], total: 0, fileSize: 0, file: '', es: null, err: '',
    };
    let models = [];
    const view = () => root.querySelector('#log-view');

    const loadModels = async () => {
      try {
        const r = await api.get('/api/models');
        models = (r.data || []).map((m) => m.id).filter(Boolean);
      } catch (e) { models = []; }
    };

    const renderShell = () => {
      root.innerHTML = renderLogsShell(st, models, ctx);
      bind();
    };

    const paint = () => {
      const v = view();
      if (!v) return;
      const list = st.entries;
      v.innerHTML = list.length ? list.map((e) => logLineHTML(e, st.q)).join('') : `<div class="empty">没有匹配的日志行</div>`;
      if (st.follow) v.scrollTop = v.scrollHeight;
      const info = root.querySelector('#log-info');
      if (info) info.textContent = `显示 ${list.length} 行 / 文件共 ${st.total || '?'} 行 · ${bytes(st.fileSize)}${st.live ? ' · 实时' : ''}`;
    };

    const load = async () => {
      const p = new URLSearchParams();
      if (st.q) p.set('q', st.q);
      if (st.kind !== 'all') p.set('kind', st.kind);
      if (st.level !== 'all') p.set('level', st.level);
      if (st.uid) p.set('uid', st.uid);
      if (st.model) p.set('model', st.model);
      p.set('limit', String(st.limit));
      try {
        const r = await api.get('/api/logs?' + p.toString());
        if (ctx.stale()) return;
        st.entries = r.entries || [];
        st.total = r.total_lines;
        st.fileSize = r.file_size;
        st.file = r.file;
        st.err = '';
        paint();
      } catch (e) {
        st.err = e.message;
        const v = view();
        if (v) v.innerHTML = `<div class="empty">${esc(e.message)}</div>`;
      }
    };

    const stopStream = () => {
      if (st.es) { st.es.close(); st.es = null; }
      st.live = false;
    };

    const startStream = () => {
      if (st.es) return;
      const es = new EventSource('/api/logs/stream');
      st.es = es;
      es.addEventListener('line', (ev) => {
        try {
          const e = JSON.parse(ev.data);
          if (!matchFilter(e, st)) return;
          st.entries.push(e);
          if (st.entries.length > st.limit) st.entries.splice(0, st.entries.length - st.limit);
          const v = view();
          if (!v) return;
          if (v.querySelector('.empty')) v.innerHTML = '';
          v.insertAdjacentHTML('beforeend', logLineHTML(e, st.q));
          if (st.follow) v.scrollTop = v.scrollHeight;
        } catch (_) { /* ignore */ }
      });
      es.addEventListener('reset', () => { st.entries = []; paint(); });
      es.addEventListener('error', () => {
        if (st.es === es && st.live) { setTimeout(() => { if (st.live) { stopStream(); st.live = true; startStream(); } }, 1500); }
      });
    };

    const bind = () => {
      const $ = (s) => root.querySelector(s);
      const q = $('#f-q'), uid = $('#f-uid');
      if (q) q.value = st.q;
      if (uid) uid.value = st.uid;
      const kindSel = $('#f-kind'); if (kindSel) kindSel.value = st.kind;
      const lvlSel = $('#f-level'); if (lvlSel) lvlSel.value = st.level;
      const mdlSel = $('#f-model'); if (mdlSel) mdlSel.value = st.model;
      const limSel = $('#f-limit'); if (limSel) limSel.value = String(st.limit);
      const follow = $('#f-follow'); if (follow) follow.checked = st.follow;
      const live = $('#f-live'); if (live) live.checked = st.live;
      paint();
    };

    const applyAndLoad = () => {
      const $ = (s) => root.querySelector(s);
      st.q = ($('#f-q') || {}).value || '';
      st.uid = ($('#f-uid') || {}).value || '';
      st.kind = ($('#f-kind') || {}).value || 'all';
      st.level = ($('#f-level') || {}).value || 'all';
      st.model = ($('#f-model') || {}).value || '';
      st.limit = Number((($('#f-limit') || {}).value) || 400);
      load();
    };

    delegate(root, {
      reload: () => { loadModels().then(renderShell); },
      apply: applyAndLoad,
      clear: () => { st.entries = []; paint(); },
      toggleFollow: () => { const c = root.querySelector('#f-follow'); st.follow = c.checked; if (st.follow) { const v = view(); if (v) v.scrollTop = v.scrollHeight; } },
      resetAll: () => { Object.assign(st, { q: '', kind: 'all', level: 'all', uid: '', model: '', limit: 400 }); renderShell(); load(); },
    });

    // 实时开关（change 事件）
    root.addEventListener('change', (ev) => {
      const t = ev.target;
      if (t.id === 'f-live') {
        st.live = t.checked;
        if (st.live) { load().then(startStream); toast('已开启实时跟随'); }
        else { stopStream(); toast('已停止实时跟随'); }
        paint();
      }
      if (t.id === 'f-follow') { st.follow = t.checked; }
      if (['f-kind', 'f-level', 'f-model', 'f-limit'].includes(t.id)) applyAndLoad();
    });
    root.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' && ['f-q', 'f-uid'].includes(ev.target.id)) applyAndLoad();
    });

    root.innerHTML = `<div class="empty">正在读取日志…</div>`;
    loadModels().then(() => { renderShell(); return load(); });
    ctx.onCleanup(() => stopStream());
  },
};

function matchFilter(e, st) {
  if (st.kind !== 'all' && e.kind !== st.kind) return false;
  if (st.level !== 'all' && e.level !== st.level) return false;
  if (st.model && e.model !== st.model) return false;
  if (st.uid && !(e.raw || '').includes(st.uid)) return false;
  if (st.q && !(e.raw || '').toLowerCase().includes(st.q.toLowerCase())) return false;
  return true;
}

function renderLogsShell(st, models, ctx) {
  const opts = (vals, cur) => vals.map(([v, l]) => `<option value="${esc(v)}" ${v === cur ? 'selected' : ''}>${esc(l)}</option>`).join('');
  return `${pageHead('日志', `<code>${esc(st.file || ctx.info.base_dir + '\\logs\\console.log')}</code> · 请求行由网关进程 stdout 重定向而来`, '')}
  <div class="log-toolbar">
    <label class="switch"><input type="checkbox" id="f-live"> 实时跟随</label>
    <label class="switch"><input type="checkbox" id="f-follow"> 自动滚动</label>
    <select id="f-kind">${opts([['all', '全部类型'], ['chat', '请求行'], ['app', '应用日志'], ['banner', '分隔行']], st.kind)}</select>
    <select id="f-level">${opts([['all', '全部级别'], ['INFO', 'INFO'], ['WARN', 'WARN'], ['ERROR', 'ERROR']], st.level)}</select>
    <select id="f-model"><option value="">全部模型</option>${models.map((m) => `<option value="${esc(m)}" ${m === st.model ? 'selected' : ''}>${esc(m)}</option>`).join('')}</select>
    <input type="text" id="f-q" placeholder="关键词（模型/昵称/错误）">
    <input type="text" id="f-uid" placeholder="uid 前 8 位">
    <select id="f-limit">${opts([['200', '200 行'], ['400', '400 行'], ['1000', '1000 行'], ['3000', '3000 行']], String(st.limit))}</select>
    <button class="btn sm" data-act="apply">应用筛选</button>
    <button class="btn sm" data-act="resetAll">重置</button>
    <button class="btn sm" data-act="clear">清屏</button>
    <a class="btn sm" href="/api/logs/download">下载日志</a>
  </div>
  <div class="hint" id="log-info" style="margin-bottom:8px"></div>
  <div class="log-view" id="log-view"></div>`;
}

function logLineHTML(e, q = '') {
  const cls = e.kind === 'chat' ? 'chat' : e.kind === 'banner' ? 'banner' : (e.level === 'ERROR' ? 'error' : e.level === 'WARN' ? 'warn' : '');
  let body;
  if (e.kind === 'chat') {
    body = `<span class="uid">#${esc(e.req_id)} ${esc(e.time)}</span> <b>${hl(e.model, q)}</b> ${esc(e.mode)} ${statusBadge(e.status)} ${hl(e.user, q)} <span class="uid">${hl(e.uid8, q)}</span> TTFB=${esc(e.ttfb_ms)}ms tok=${esc(e.tokens)} ${flt(e.tok_per_sec, 1)} tok/s total=${flt(e.total_s, 2)}s`;
  } else {
    body = hl(e.text || e.raw, q);
  }
  return `<div class="log-line ${cls}"><span class="ln">${esc(e.line)}</span><span class="body">${body}</span></div>`;
}

// ============================================================ 配置

export const config = {
  id: 'config', label: '配置', icon: '⚙',
  mount(root, ctx) {
    const load = async () => {
      const d = await all({ cfg: api.get('/api/config'), panel: api.get('/api/panel/config'), info: api.get('/api/panel/info') });
      if (ctx.stale()) return;
      root.innerHTML = renderConfig(d, ctx);
    };
    root.innerHTML = `<div class="empty">正在读取配置…</div>`;
    load().catch((e) => { console.error(e); if (!ctx.stale()) root.innerHTML = errorCard('加载失败：' + e.message); });

    delegate(root, {
      reload: () => root.dispatchEvent(new CustomEvent('wb:reload')),
      format: () => {
        const ta = root.querySelector('#cfg-text');
        try { ta.value = JSON.stringify(JSON.parse(ta.value), null, 2); toast('已格式化'); } catch (e) { toastErr('JSON 解析失败：' + e.message); }
      },
      validate: () => {
        const ta = root.querySelector('#cfg-text');
        try { JSON.parse(ta.value); toast('JSON 语法正确'); } catch (e) { toastErr('JSON 解析失败：' + e.message); }
      },
      enableAdmin: async (el) => {
        const ta = root.querySelector('#cfg-text');
        const okc = await confirmModal('开启管理接口', '将在 config.json 写入 <code>admin.enabled=true</code> 与 <code>admin.audit_enabled=true</code>（面板的禁用/启用/复活、手动触发任务依赖它），并备份原文件。改完需要重启网关生效。', { okLabel: '写入并备份' });
        if (!okc) return;
        busy(el);
        try {
          const obj = JSON.parse(ta.value);
          obj.admin = Object.assign({ audit_file: './data/admin_audit.log' }, obj.admin || {}, { enabled: true, audit_enabled: true });
          ta.value = JSON.stringify(obj, null, 2);
          const r = await api.put('/api/config', { text: ta.value });
          toast('已写入（备份 ' + (r.backup || '').split('\\').pop() + '）');
          const again = await confirmModal('是否立即重启网关？', '配置改动只有重启网关才生效。重启会造成 1~5 秒的请求中断。', { okLabel: '立即重启' });
          if (again) { await api.post('/api/service/restart'); toast('网关已重启'); }
          root.dispatchEvent(new CustomEvent('wb:reload'));
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
      enableMetrics: async (el) => {
        const ta = root.querySelector('#cfg-text');
        busy(el);
        try {
          const obj = JSON.parse(ta.value);
          obj.metrics = Object.assign({}, obj.metrics || {}, { enabled: true });
          ta.value = JSON.stringify(obj, null, 2);
          const r = await api.put('/api/config', { text: ta.value });
          toast('已开启 metrics 并写入（备份 ' + (r.backup || '').split('\\').pop() + '）；重启网关后生效');
          root.dispatchEvent(new CustomEvent('wb:reload'));
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
      saveCfg: async (el) => {
        const ta = root.querySelector('#cfg-text');
        try { JSON.parse(ta.value); } catch (e) { toastErr('JSON 语法错误，未保存：' + e.message); return; }
        const okc = await confirmModal('保存 config.json', '将先备份原文件为 <code>config.json.bak-时间戳</code>，再写入新内容。', { okLabel: '保存' });
        if (!okc) return;
        busy(el);
        try {
          const r = await api.put('/api/config', { text: ta.value });
          toast('已保存，备份：' + (r.backup || '').split('\\').pop());
          root.dispatchEvent(new CustomEvent('wb:reload'));
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
      savePanel: async (el) => {
        busy(el);
        try {
          const body = {
            listen: root.querySelector('#p-listen').value.trim(),
            base_url: root.querySelector('#p-base').value.trim(),
            refresh_ms: Number(root.querySelector('#p-refresh').value || 5000),
            start_script: root.querySelector('#p-startscr').value.trim(),
            dir: root.querySelector('#p-dir').value.trim(),
          };
          const pw = root.querySelector('#p-pass').value;
          if (pw) body.password = pw;
          const r = await api.put('/api/panel/config', body);
          toast(r.note || '面板配置已保存');
          root.dispatchEvent(new CustomEvent('wb:reload'));
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
      copy: (el) => copy(el.dataset.text || '', '已复制'),
    });
  },
};

function renderConfig(d, ctx) {
  const parts = [];
  parts.push(pageHead('配置', '面板设置与网关 config.json（写入前自动备份）', `<button class="btn" data-act="reload">刷新</button>`));
  const info = ctx.info;
  const pc = !isErr(d.panel) && d.panel ? d.panel.config : null;
  const gwc = pc ? pc.gateway || {} : {};
  const uic = pc ? pc.ui || {} : {};
  parts.push(`<div class="section"><div class="section-title">面板设置</div><div class="card">
    <div class="form-row">
      <label class="field"><span>面板监听地址</span><input type="text" id="p-listen" value="${esc((pc && pc.listen) || info.listen)}"><em>改动需重启面板进程生效</em></label>
      <label class="field"><span>网关地址</span><input type="text" id="p-base" value="${esc(gwc.base_url || info.gateway)}"></label>
    </div>
    <div class="form-row">
      <label class="field"><span>自动刷新间隔 (ms)</span><input type="number" id="p-refresh" min="1000" step="500" value="${esc(uic.refresh_ms || info.ui.refresh_ms)}"></label>
      <label class="field"><span>访问密码</span><input type="password" id="p-pass" placeholder="${info.auth_required ? '已设置，留空表示不修改' : '留空 = 不启用登录（仅建议本机使用）'}"></label>
    </div>
    <label class="field"><span>网关启动脚本</span><input type="text" id="p-startscr" value="${esc(gwc.start_script || '')}"><em>服务页的「启动」按钮即执行它（默认 D:\\Program\\autostart\\run-workbuddy2api.bat，自带日志重定向）</em></label>
    <label class="field"><span>网关目录</span><input type="text" id="p-dir" value="${esc(gwc.dir || info.base_dir)}"></label>
    <div class="row"><div class="spacer"></div><button class="btn primary" data-act="savePanel">保存面板设置</button></div>
    <div class="kv" style="margin-top:12px">
      <dt>配置文件</dt><dd><code>${esc(info.config_file)}</code></dd>
      <dt>面板版本</dt><dd>v${esc(info.version)} · 启动于 ${esc(ts(info.started_at))}</dd>
      <dt>登录</dt><dd>${info.auth_required ? '已启用' : '未启用（仅监听本机时安全）'}</dd>
      <dt>日志文件</dt><dd><code>${esc(gwc.log_file || '')}</code></dd>
      <dt>账号目录</dt><dd><code>${esc(gwc.auth_dir || '')}</code> · 暂停区 <code>${esc(gwc.paused_dir || '')}</code></dd>
    </div>
  </div></div>`);

  const cfg = isErr(d.cfg) ? null : d.cfg;
  const view = cfg ? cfg.config : null;
  const summary = cfg ? cfg.summary : null;
  if (!view) {
    parts.push(errorCard('读取 config.json 失败：' + (d.cfg.__error || '未知错误')));
  } else {
    const s = summary || {};
    parts.push(`<div class="section"><div class="section-title">网关 config.json
      ${s.admin_enabled ? badge('admin 已开启', 'ok') : badge('admin 未开启', 'warn')}
      ${s.metrics_enabled ? badge('metrics 已开启', 'ok') : badge('metrics 未开启', '')}
      ${view.valid ? badge('JSON 有效', 'ok') : badge('JSON 无效', 'danger')}
    </div>
    <div class="card">
      <div class="row wrap" style="margin-bottom:10px">
        <span class="hint"><code>${esc(view.path)}</code> · ${bytes(view.size)} · 修改于 ${esc(ts(view.mod_time * 1000))}</span>
        <div class="spacer"></div>
        <button class="btn sm" data-act="validate">校验</button>
        <button class="btn sm" data-act="format">格式化</button>
        <button class="btn sm" data-act="enableAdmin">一键开启 admin</button>
        <button class="btn sm" data-act="enableMetrics">开启 metrics</button>
        <button class="btn sm primary" data-act="saveCfg">保存</button>
      </div>
      ${view.error ? `<div class="alert danger" style="margin-bottom:10px">${esc(view.error)}</div>` : ''}
      <textarea class="json" id="cfg-text" spellcheck="false">${esc(view.text)}</textarea>
      <div class="kv" style="margin-top:12px">
        <dt>listen</dt><dd>${esc(s.listen || '—')}</dd>
        <dt>api_key</dt><dd><code>${esc(s.api_key_masked || '未设置')}</code>（面板服务端注入，不会下发到浏览器）</dd>
        <dt>审计文件</dt><dd><code>${esc((ctx.info.gateway_config && ctx.info.gateway_config.admin && ctx.info.gateway_config.admin.audit_file) || './data/admin_audit.log')}</code></dd>
      </div>
      ${view.backups && view.backups.length ? `<details class="inline" style="margin-top:12px"><summary>历史备份（${view.backups.length}）</summary>
        ${table([{ label: '文件' }, { label: '大小', cls: 'num' }, { label: '时间' }],
          view.backups.map((b) => [`<span class="mono">${esc(b.name)}</span>`, bytes(b.size), esc(ts(b.mod_time * 1000))]))}</details>` : ''}
    </div></div>`);
  }

  const gw = d.panel && d.panel.gateway_config ? d.panel.gateway_config : null;
  parts.push(`<div class="section"><div class="section-title">网关配置（脱敏副本，实时来自文件）</div><div class="card">
    ${gw && gw.error ? `<div class="alert danger">${esc(gw.error)}</div>` : jsonBlock(gw || {})}
  </div></div>`);
  return parts.join('');
}

// ============================================================ 服务

export const service = {
  id: 'service', label: '服务', icon: '⏻',
  mount(root, ctx) {
    const load = async () => {
      const d = await all({ svc: api.get('/api/service'), tasks: api.get('/api/tasks'), audit: api.get('/api/audit?limit=100') });
      if (ctx.stale()) return;
      root.innerHTML = renderService(d, ctx);
    };
    root.innerHTML = `<div class="empty">正在读取服务状态…</div>`;
    load().catch((e) => { console.error(e); if (!ctx.stale()) root.innerHTML = errorCard('加载失败：' + e.message); });
    ctx.every(load, Math.max(4000, ctx.info.ui.refresh_ms || 5000));
    ctx.onReload(load);

    delegate(root, {
      reload: () => root.dispatchEvent(new CustomEvent('wb:reload')),
      svc: async (el) => {
        const action = el.dataset.action;
        const label = { start: '启动', stop: '停止', restart: '重启', reload: '重新读取配置' }[action] || action;
        if (action === 'stop' || action === 'restart') {
          const okc = await confirmModal('确认' + label, action === 'stop'
            ? '停止网关后，所有依赖它的程序（含 new-api 与 DSH 会话）都会请求失败，直到重新启动。'
            : '重启网关会造成 1~5 秒的请求中断。', { danger: action === 'stop', okLabel: label });
          if (!okc) return;
        }
        busy(el);
        try {
          const r = await api.post('/api/service/' + action);
          toast(r.message || label + '完成');
          setTimeout(() => root.dispatchEvent(new CustomEvent('wb:reload')), 1200);
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
      runTask: async (el) => {
        const name = el.dataset.name;
        busy(el);
        try {
          const r = await api.post(`/api/tasks/${encodeURIComponent(name)}/run`);
          toast(r.note || (name + ' 已触发'));
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
    });
  },
};

function renderService(d, ctx) {
  const svc = isErr(d.svc) ? null : d.svc;
  const s = svc ? svc.service : null;
  const parts = [];
  parts.push(pageHead('服务', '网关进程的启停与运行环境', `<button class="btn" data-act="reload">刷新</button>`));
  if (!svc) {
    parts.push(errorCard('读取服务状态失败：' + d.svc.__error));
  } else {
    const h = svc.health || {};
    parts.push(`<div class="grid kpis section">
      ${kpi('进程状态', s.running ? `<span style="color:var(--ok)">运行中</span>` : `<span style="color:var(--danger)">未运行</span>`, s.running ? `PID ${num(s.pid)} · 端口 ${num(s.port)}` : '端口无监听进程')}
      ${kpi('健康检查', svc.reachable ? (svc.health_status === 200 ? '正常' : 'HTTP ' + svc.health_status) : '无法连接', svc.health_error ? esc(svc.health_error) : `健康账号 ${num(h.healthy)}/${num(h.total)}`)}
      ${kpi('日志行数', num(svc.log_lines), s.log_file ? bytes(s.log_file.size) : '')}
      ${kpi('服务器二进制', s.server_exe && s.server_exe.exists ? bytes(s.server_exe.size) : '缺失', s.server_exe ? esc(ts(s.server_exe.mod_time * 1000)) : '')}
    </div>`);

    if (!ctx.info.admin_enabled) parts.push(`<div class="alert warn" style="margin-top:14px">admin.enabled=false：任务手动触发与账号禁用接口不可用（<a href="#/config">配置页一键开启</a>）。</div>`);
    if (svc.health_error) parts.push(`<div class="alert danger" style="margin-top:10px">${esc(svc.health_error)}</div>`);
    else if (h.realm_servable) parts.push(`<div class="alert ${h.healthy ? 'ok' : 'warn'}" style="margin-top:10px">realm 可服务：CN ${h.realm_servable.cn ? '✓' : '✗'} · Global ${h.realm_servable.global ? '✓' : '✗'} · 服务标识 <code>${esc(h.service || '')}</code></div>`);

    parts.push(`<div class="section"><div class="section-title">进程控制</div><div class="card">
      <div class="row wrap">
        <button class="btn primary" data-act="svc" data-action="start" ${s.running ? 'disabled' : ''}>启动</button>
        <button class="btn danger" data-act="svc" data-action="stop" ${s.running ? '' : 'disabled'}>停止</button>
        <button class="btn" data-act="svc" data-action="restart">重启</button>
        <button class="btn" data-act="svc" data-action="reload">重新读取配置</button>
        <div class="spacer"></div>
        <span class="hint">启动脚本：<code>${esc(s.start_script)}</code>${s.start_script_exists ? '' : ' <span class="badge danger">不存在</span>'}</span>
      </div>
      <div class="kv" style="margin-top:12px">
        <dt>监听</dt><dd><code>${esc(s.listen)}</code>（端口 ${num(s.port)}）</dd>
        <dt>工作目录</dt><dd><code>${esc(s.base_dir)}</code></dd>
        <dt>服务器</dt><dd><code>${esc(s.server_exe.path)}</code> ${s.server_exe.exists ? '' : badge('缺失', 'danger')} ${s.server_exe.exists ? bytes(s.server_exe.size) + ' · ' + esc(ts(s.server_exe.mod_time * 1000)) : ''}</dd>
        <dt>登录工具</dt><dd><code>${esc(s.login_exe.path)}</code> ${s.login_exe.exists ? '' : badge('缺失', 'danger')} ${s.login_exe.exists ? bytes(s.login_exe.size) + ' · ' + esc(ts(s.login_exe.mod_time * 1000)) : ''}</dd>
        <dt>配置文件</dt><dd><code>${esc(s.config_file.path)}</code> ${s.config_file.exists ? bytes(s.config_file.size) + ' · ' + esc(ts(s.config_file.mod_time * 1000)) : badge('缺失', 'danger')}</dd>
        <dt>日志文件</dt><dd><code>${esc(s.log_file.path)}</code> ${s.log_file.exists ? bytes(s.log_file.size) : badge('缺失', 'danger')}</dd>
      </div>
    </div></div>`);
  }

  const tasks = isErr(d.tasks) ? null : d.tasks;
  if (tasks && tasks.tasks) {
    parts.push(`<div class="section"><div class="section-title">定时任务</div><div class="grid cards">${tasks.tasks.map(taskCard).join('')}</div></div>`);
  }

  const audit = isErr(d.audit) ? null : d.audit;
  if (audit) {
    const items = (audit.items || []).slice().reverse().map((raw) => {
      let o = raw;
      if (typeof raw === 'string') { try { o = JSON.parse(raw); } catch (e) { o = { raw }; } }
      return [`<span class="mono">${esc(o.ts || '')}</span>`, esc(o.action || ''), `<span class="mono">${esc(o.target || '')}</span>`, o.status === 'ok' || o.status === 200 ? badge('ok', 'ok') : badge(String(o.status || ''), 'warn'), esc(o.remote || ''), `<span class="mono">${esc(o.key || '')}</span>`, esc(o.reason || '')];
    });
    parts.push(`<div class="section"><div class="section-title">管理操作审计（${(audit.items || []).length}）</div>
      ${table([{ label: '时间' }, { label: '动作' }, { label: '目标' }, { label: '结果' }, { label: '来源' }, { label: 'key' }, { label: '原因' }], items, '暂无审计记录（开启 admin.audit_enabled 后记录）')}
      <div class="hint" style="margin-top:6px"><code>${esc(audit.file || '')}</code>${audit.note ? ' · ' + esc(audit.note) : ''}</div></div>`);
  }
  return parts.join('');
}

// ============================================================ API 配置

const ACCESS_TABS = [
  { id: 'basics', label: '接入信息' },
  { id: 'models', label: '模型与定价' },
  { id: 'snippets', label: '代码示例' },
  { id: 'clients', label: '客户端配置' },
];

// 代码示例的三协议 × 三语言模板。$BASE 为接入地址；密钥用环境变量占位，
// 不把真实密钥写进示例（用户复制后自行替换）。
const SNIPPETS = {
  chat: {
    label: 'OpenAI Chat',
    base: '/v1/chat/completions',
    curl: (b) => `curl ${b}/v1/chat/completions \\
  -H "Authorization: Bearer $WB_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "glm-5.2",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "你好"}
    ],
    "stream": false
  }'`,
    python: (b) => `from openai import OpenAI

client = OpenAI(base_url="${b}/v1", api_key="$WB_API_KEY")

resp = client.chat.completions.create(
    model="glm-5.2",
    messages=[
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": "你好"},
    ],
)
print(resp.choices[0].message.content)`,
    node: (b) => `import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "${b}/v1",
  apiKey: process.env.WB_API_KEY,
});

const resp = await client.chat.completions.create({
  model: "glm-5.2",
  messages: [
    { role: "system", content: "You are a helpful assistant." },
    { role: "user", content: "你好" },
  ],
});
console.log(resp.choices[0].message.content);`,
  },
  responses: {
    label: 'OpenAI Responses',
    base: '/v1/responses',
    curl: (b) => `curl ${b}/v1/responses \\
  -H "Authorization: Bearer $WB_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "glm-5.2",
    "input": "用一句话介绍你自己",
    "max_output_tokens": 512
  }'`,
    python: (b) => `from openai import OpenAI

client = OpenAI(base_url="${b}/v1", api_key="$WB_API_KEY")

resp = client.responses.create(
    model="glm-5.2",
    input="用一句话介绍你自己",
    max_output_tokens=512,
)
print(resp.output_text)`,
    node: (b) => `import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "${b}/v1",
  apiKey: process.env.WB_API_KEY,
});

const resp = await client.responses.create({
  model: "glm-5.2",
  input: "用一句话介绍你自己",
  max_output_tokens: 512,
});
console.log(resp.output_text);`,
  },
  anthropic: {
    label: 'Anthropic',
    base: '/v1/messages',
    curl: (b) => `curl ${b}/v1/messages \\
  -H "x-api-key: $WB_API_KEY" \\
  -H "anthropic-version: 2023-06-01" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "glm-5.2",
    "max_tokens": 512,
    "messages": [
      {"role": "user", "content": "你好"}
    ]
  }'`,
    python: (b) => `import anthropic

client = anthropic.Anthropic(
    base_url="${b}",
    api_key="$WB_API_KEY",
)

msg = client.messages.create(
    model="glm-5.2",
    max_tokens=512,
    messages=[{"role": "user", "content": "你好"}],
)
print(msg.content[0].text)`,
    node: (b) => `import Anthropic from "@anthropic-ai/sdk";

const client = new Anthropic({
  baseURL: "${b}",
  apiKey: process.env.WB_API_KEY,
});

const msg = await client.messages.create({
  model: "glm-5.2",
  max_tokens: 512,
  messages: [{ role: "user", content: "你好" }],
});
console.log(msg.content[0].text);`,
  },
};

// highlightCode 轻量语法高亮：正则分词后逐段 escape 上色（先 tokenize 后 escape，
// 不会引入 XSS）。支持注释 / 字符串 / 关键字 / 数字 / CLI 旗标，覆盖 bash/python/js。
const HL_RE = /("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|#[^\n]*|\b(?:curl|export|from|import|const|await|async|print|new|function|return|true|false|null)\b|--[a-zA-Z-]+|\b\d+(?:\.\d+)?\b)/g;
function highlightCode(code) {
  const out = [];
  let last = 0, m;
  HL_RE.lastIndex = 0;
  while ((m = HL_RE.exec(code)) !== null) {
    out.push(esc(code.slice(last, m.index)));
    const t = m[0];
    const cls = t.startsWith('#') ? 'tk-c'
      : t.startsWith('"') || t.startsWith("'") ? 'tk-s'
      : t.startsWith('--') ? 'tk-f'
      : /^\d/.test(t) ? 'tk-n' : 'tk-k';
    out.push(`<i class="${cls}">${esc(t)}</i>`);
    last = m.index + t.length;
  }
  out.push(esc(code.slice(last)));
  return out.join('');
}

function codeBlock(code, title, act) {
  return `<div class="code-wrap">
    <div class="code-head"><span>${esc(title)}</span><div class="spacer"></div>${act ? `<button class="btn ghost sm" data-act="copySnippet">${act}</button>` : ''}</div>
    <pre class="code-block"><code>${highlightCode(code)}</code></pre>
  </div>`;
}

export const access = {
  id: 'access', label: 'API 配置', icon: '◈',
  mount(root, ctx) {
    const st = { tab: 'basics', acc: null, modelsErr: null, models: [], q: '', proto: 'chat', lang: 'curl', revealed: {} };
    const load = async () => {
      const d = await all({ acc: api.get('/api/access'), models: api.get('/api/models') });
      if (ctx.stale()) return;
      st.acc = isErr(d.acc) ? null : d.acc;
      st.accErr = isErr(d.acc) ? d.acc.__error : '';
      st.modelsErr = isErr(d.models) ? d.models.__error : '';
      const md = (!isErr(d.models) && d.models && Array.isArray(d.models.data)) ? d.models.data : [];
      st.models = md;
      render();
    };
    const render = () => { root.innerHTML = renderAccess(st, ctx); };
    root.innerHTML = `<div class="empty">正在读取接入信息…</div>`;
    load().catch((e) => { console.error(e); if (!ctx.stale()) root.innerHTML = errorCard('加载失败：' + e.message); });
    ctx.onReload(load);
    ctx.every(load, 60000); // 接入信息变化频率低（改配置才变），60s 兜底刷新

    delegate(root, {
      reload: () => root.dispatchEvent(new CustomEvent('wb:reload')),
      tab: (el) => { st.tab = el.dataset.tab; render(); },
      proto: (el) => { st.proto = el.dataset.proto; render(); },
      lang: (el) => { st.lang = el.dataset.lang; render(); },
      '@mq': (el) => { st.q = el.value; render(); },
      copyText: (el) => copy(el.dataset.text || el.dataset.url || '', '已复制到剪贴板'),
      reveal: async (el) => {
        const name = el.dataset.name;
        busy(el);
        try {
          const r = await api.post('/api/access/reveal', { name });
          st.revealed[name] = r.key;
          render();
        } catch (e) { toastErr(e); busy(el, false); }
      },
      hide: (el) => { delete st.revealed[el.dataset.name]; render(); },
      copyKey: async (el) => {
        const name = el.dataset.name;
        busy(el);
        try {
          const r = await api.post('/api/access/reveal', { name });
          copy(r.key, `密钥 ${name} 已复制（取用已记录在面板日志）`);
        } catch (e) { toastErr(e); }
        busy(el, false);
      },
      copySnippet: (el) => {
        const proto = SNIPPETS[st.proto] || SNIPPETS.chat;
        const base = (st.acc && st.acc.base_url) || 'http://127.0.0.1:7863';
        copy(proto[st.lang](base), '代码已复制');
      },
      clientCopy: (el) => copy(el.dataset.text || '', '配置已复制'),
    });
  },
};

function lanBaseURL(base) {
  // 用当前访问页面的 hostname 推导局域网地址（面板与网关同机部署）。
  try {
    const u = new URL(base);
    if (location.hostname && location.hostname !== '127.0.0.1' && location.hostname !== 'localhost') {
      return `${location.protocol}//${location.hostname}:${u.port}`;
    }
    return '';
  } catch (e) { return ''; }
}

function renderAccess(st, ctx) {
  const parts = [];
  parts.push(pageHead('API 配置', '把网关接入到你的应用 / 客户端：接入地址 · 密钥 · 模型 · 代码示例',
    `<button class="btn" data-act="reload">刷新</button>`));
  parts.push(`<div class="seg access-tabs">${ACCESS_TABS.map((t) =>
    `<button class="${st.tab === t.id ? 'active' : ''}" data-act="tab" data-tab="${t.id}">${esc(t.label)}</button>`).join('')}</div>`);
  if (!st.acc) {
    parts.push(`<div class="section">${errorCard('读取接入信息失败：' + esc(st.accErr || '未知错误'), '网关 config.json 不可读时本页不可用，请先在服务页确认网关已启动。')}</div>`);
    return parts.join('');
  }
  if (st.tab === 'basics') parts.push(renderBasics(st));
  else if (st.tab === 'models') parts.push(renderModelsTab(st));
  else if (st.tab === 'snippets') parts.push(renderSnippets(st));
  else parts.push(renderClients(st));
  return parts.join('');
}

function renderBasics(st) {
  const acc = st.acc;
  const lan = lanBaseURL(acc.base_url);
  const parts = [];
  // 接入地址
  parts.push(`<div class="section"><div class="section-title">接入地址</div><div class="card">
    <div class="kv">
      <dt>本机</dt><dd class="link-box"><code>${esc(acc.base_url)}</code>
        <button class="btn sm" data-act="copyText" data-text="${esc(acc.base_url)}">复制</button></dd>
      ${lan ? `<dt>局域网</dt><dd class="link-box"><code>${esc(lan)}</code>
        <button class="btn sm" data-act="copyText" data-text="${esc(lan)}">复制</button>
        <span class="hint">按当前页面主机名推导，远程调用以实际可达地址为准</span></dd>` : ''}
      <dt>网关监听</dt><dd><code>${esc(acc.listen || '')}</code></dd>
    </div>
  </div></div>`);
  // 端点清单
  const rows = (acc.protocols || []).map((p) => [
    badge(p.protocol, p.protocol === 'Anthropic' ? 'purple' : p.protocol === '通用' ? '' : 'info'),
    `<span class="mono">${esc(p.path)}</span> <button class="btn ghost sm" data-act="copyText" data-text="${esc(acc.base_url + p.path)}">复制</button>`,
    esc(p.desc),
  ]);
  parts.push(`<div class="section"><div class="section-title">端点清单</div>${table([{ label: '协议' }, { label: '端点' }, { label: '说明' }], rows, '无端点')}</div>`);
  // 密钥
  const keys = acc.keys || [];
  if (!keys.length) {
    parts.push(`<div class="section"><div class="section-title">API 密钥</div>
      <div class="card"><div class="alert warn">网关未配置 <code>api_key</code>（不鉴权模式）。生产 / 局域网环境建议在<a href="#/config">配置页</a>设置密钥。</div></div></div>`);
  } else {
    const rows = keys.map((k) => {
      const shown = st.revealed[k.main ? 'main' : k.name];
      const nm = k.main ? 'main' : k.name;
      return [`<b>${esc(k.name)}</b>${k.main ? ' ' + badge('主密钥', 'ok') : ''}${(k.groups || []).length ? ' ' + k.groups.map((g) => badge('分组: ' + g, 'info')).join(' ') : ''}`,
        `<code class="key-mask">${shown ? esc(shown) : esc(k.masked)}</code>`,
        `<span class="row wrap" style="gap:6px">
          ${shown
            ? `<button class="btn sm" data-act="hide" data-name="${esc(nm)}">隐藏</button>`
            : `<button class="btn sm" data-act="reveal" data-name="${esc(nm)}">显示</button>`}
          <button class="btn sm primary" data-act="copyKey" data-name="${esc(nm)}">复制</button>
        </span>`];
    });
    parts.push(`<div class="section"><div class="section-title">API 密钥</div>${table([{ label: '名称' }, { label: '密钥' }, { label: '操作' }], rows)}
      <div class="hint" style="margin-top:8px">「显示 / 复制」会把密钥明文经面板后端取出一次，取用记录在面板运行日志中；密钥本体保存在网关 <code>config.json</code>，<a href="#/config">配置页</a>可增删分组密钥（改动后重启网关生效）。</div></div>`);
  }
  return parts.join('');
}

function renderModelsTab(st) {
  const parts = [];
  parts.push(`<div class="log-toolbar" style="margin-bottom:12px">
    <input type="text" placeholder="搜索模型 id / 描述…" data-change="mq" value="${esc(st.q)}" style="min-width:260px">
    <span class="hint">共 ${num(st.models.length)} 个模型（来自网关 /v1/models，动态）</span>
  </div>`);
  if (st.modelsErr) {
    parts.push(errorCard('读取模型列表失败：' + esc(st.modelsErr), '网关未启动或无健康账号时模型列表为空。'));
    return parts.join('');
  }
  const q = st.q.trim().toLowerCase();
  const list = st.models.filter((m) => !q || String(m.id || '').toLowerCase().includes(q) || String(m.description || '').toLowerCase().includes(q) || String(m.name || '').toLowerCase().includes(q));
  const rows = list.map((m) => {
    const caps = [];
    if (m.supports_images) caps.push(badge('图片', 'info'));
    if (m.supports_tool_call) caps.push(badge('工具', 'ok'));
    if (m.supports_reasoning || m.reasoning_supported_efforts) caps.push(badge('推理', 'purple'));
    return [
      `<span class="mono">${esc(m.id)}</span> <button class="btn ghost sm" data-act="copyText" data-text="${esc(m.id)}">复制</button>`,
      esc(m.name || ''),
      esc(m.credits || '—'),
      m.context_length ? num(m.context_length) : '—',
      m.max_output_tokens ? num(m.max_output_tokens) : '—',
      m.reasoning_supported_efforts ? `<span class="mono">${esc((m.reasoning_supported_efforts || []).join(' / '))}</span>` : '—',
      caps.join(' ') || '—',
    ];
  });
  parts.push(`${table([
    { label: '模型 id' }, { label: '名称' }, { label: '倍率' }, { label: '上下文', cls: 'num' },
    { label: '最大输出', cls: 'num' }, { label: '推理档位' }, { label: '能力' },
  ], rows, q ? '没有匹配的模型' : '暂无模型（网关动态列表为空）')}`);
  parts.push(`<div class="hint" style="margin-top:8px">倍率为上游积分计费原文（如 <code>x0.05</code>）；「模型 id」可直接用于请求体的 <code>model</code> 字段，支持 <code>cn:</code> / <code>global:</code> 前缀选域。</div>`);
  return parts.join('');
}

function renderSnippets(st) {
  const proto = SNIPPETS[st.proto] || SNIPPETS.chat;
  const base = st.acc.base_url;
  const code = proto[st.lang] ? proto[st.lang](base) : '';
  const langTabs = [['curl', 'cURL'], ['python', 'Python'], ['node', 'Node.js']];
  return `<div class="section">
    <div class="row wrap" style="gap:8px;margin-bottom:10px">
      <div class="seg">${Object.entries(SNIPPETS).map(([id, p]) =>
        `<button class="${st.proto === id ? 'active' : ''}" data-act="proto" data-proto="${id}">${esc(p.label)}</button>`).join('')}</div>
      <div class="seg">${langTabs.map(([id, label]) =>
        `<button class="${st.lang === id ? 'active' : ''}" data-act="lang" data-lang="${id}">${esc(label)}</button>`).join('')}</div>
      <span class="hint">示例使用占位密钥 <code>$WB_API_KEY</code>，替换为「接入信息」页的真实密钥</span>
    </div>
    ${codeBlock(code, `${proto.label} · ${langTabs.find(([id]) => id === st.lang)?.[1] || st.lang} · ${proto.base}`, '复制代码')}
    <div class="hint" style="margin-top:8px">${st.proto === 'anthropic'
      ? 'Anthropic 协议用 <code>x-api-key</code> 头携带密钥；SDK 的 <code>base_url</code> 填网关根地址（不带 /v1）。'
      : 'OpenAI 系 SDK 的 <code>base_url</code> 填 <code>' + esc(base) + '/v1</code>（SDK 会自动拼端点）。'}</div>
  </div>`;
}

function renderClients(st) {
  const b = st.acc.base_url;
  const key = '$WB_API_KEY';
  const blocks = [
    {
      title: 'Claude Code（Anthropic 协议）',
      text: `export ANTHROPIC_BASE_URL="${b}"\nexport ANTHROPIC_AUTH_TOKEN="${key}"\nexport ANTHROPIC_MODEL="glm-5.2"`,
      note: 'Bash/Zsh 写入 <code>~/.bashrc</code> 或会话内直接 export；Windows PowerShell 用 <code>$env:ANTHROPIC_BASE_URL="…"</code>。也可用 <code>model_alias</code> 配置把 claude-* 模型名映射到网关模型。',
    },
    {
      title: 'Codex CLI（Responses 协议）',
      text: `# ~/.codex/config.toml\nmodel = "glm-5.2"\nmodel_provider = "wb2api"\n\n[model_providers.wb2api]\nname = "workbuddy2api"\nbase_url = "${b}/v1"\nwire_api = "responses"\nenv_key = "WB2API_API_KEY"\n\n# 然后：export WB2API_API_KEY="${key}"`,
      note: '网关无响应存储，请保持 Codex 默认的 <code>store=false</code> 整体重发模式（不受影响）。',
    },
    {
      title: '通用 OpenAI 客户端（Chat 协议）',
      text: `Base URL: ${b}/v1\nAPI Key:  ${key}\nModel:    glm-5.2`,
      note: '适用于 CherryStudio、沉浸式翻译、LobeChat 等一切支持自定义 OpenAI 兼容端点的工具。',
    },
  ];
  return `<div class="grid cards section">${blocks.map((c) => `
    <div class="card">
      <h3>${esc(c.title)}</h3>
      <div style="margin:10px 0">${codeBlock(c.text, '配置', '')}</div>
      <div class="hint">${c.note}</div>
      <div style="margin-top:10px"><button class="btn sm primary" data-act="clientCopy" data-text="${esc(c.text)}">复制配置</button></div>
    </div>`).join('')}</div>`;
}

export const pages = [overview, access, accounts, stats, logs, config, service];
