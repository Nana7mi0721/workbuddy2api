// charts.js — 统计页图表：零依赖 canvas（折线/堆叠柱/单柱/环形）+ DOM 热力图。
// 全部 DPR 感知、主题色取 CSS 变量（切主题后由页面负责重绘）。

export const PALETTE = [
  '#4d6bfe', '#31a46c', '#7c5cfc', '#e5484d', '#e79008',
  '#0ea5a5', '#2e90fa', '#b568ea', '#8a93a8', '#d6455f',
];

export function cssVar(name, fallback) {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return v || fallback;
}

export function fmtInt(n) {
  return Math.round(Number(n) || 0).toLocaleString('en-US');
}

// fmtTokens 大数压缩：亿 / 万，与趋势图轴、环形图中心共用。
export function fmtTokens(n) {
  n = Number(n) || 0;
  if (Math.abs(n) >= 1e8) return (n / 1e8).toFixed(2) + ' 亿';
  if (Math.abs(n) >= 1e4) return (n / 1e4).toFixed(1) + ' 万';
  return fmtInt(n);
}

export function fmtCredit(n) {
  return (Number(n) || 0).toFixed(2);
}

// fmtAxis 图表轴标签的紧凑数字：1.2亿 / 340万 / 8,400 / 5。
export function fmtAxis(n) {
  n = Number(n) || 0;
  const a = Math.abs(n);
  if (a >= 1e8) return trimZero(n / 1e8) + '亿';
  if (a >= 1e6) return trimZero(n / 1e6) + 'M';
  if (a >= 1e4) return trimZero(n / 1e4) + '万';
  if (a >= 1000) return trimZero(n / 1000) + 'k';
  return fmtInt(n);
}
const trimZero = (x) => {
  const s = x.toFixed(1);
  return s.endsWith('.0') ? s.slice(0, -2) : s;
};

// niceMax 把轴上限取整到 1/2/5×10^k，网格线读起来是整数。
export function niceMax(v) {
  if (!(v > 0)) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 5, 10]) {
    if (m * exp >= v) return m * exp;
  }
  return 10 * exp;
}

// prepCanvas 按 DPR 设定物理像素并返回 2D 上下文；cv 需要 CSS 宽度已布局。
function prepCanvas(cv, hCss) {
  const dpr = window.devicePixelRatio || 1;
  const w = cv.clientWidth || cv.parentElement.clientWidth || 300;
  cv.width = Math.round(w * dpr);
  cv.height = Math.round(hCss * dpr);
  cv.style.height = hCss + 'px';
  const g = cv.getContext('2d');
  g.scale(dpr, dpr);
  g.clearRect(0, 0, w, hCss);
  return { g, w, h: hCss };
}

function axisFont() {
  return '10.5px ' + cssVar('--mono', 'monospace');
}

// xLabelIdx 均匀取 ~count 个 x 轴刻度下标（首尾包含）。
function xLabelIdx(len, count) {
  if (len <= count) return [...Array(len).keys()];
  const out = new Set();
  for (let i = 0; i < count; i++) out.add(Math.round((i * (len - 1)) / (count - 1)));
  return [...out].sort((a, b) => a - b);
}

// smoothPath 单调三次插值（Fritsch–Carlson）→ 贝塞尔，平滑观感对齐 ZCode 趋势线。
// 不用 Catmull-Rom：它在「长平线 + 单点突刺」时控制点（p2-(p3-p1)/6）会冲出数据
// 范围，曲线肉眼可见地掉到 x 轴以下。单调插值把每段切线夹在相邻差商之间、极值点
// 切线取平，任何输入下曲线都不会越过 [min, max] 数据包络。
function smoothPath(g, pts) {
  const n = pts.length;
  g.moveTo(pts[0].x, pts[0].y);
  if (n < 3) {
    for (let i = 1; i < n; i++) g.lineTo(pts[i].x, pts[i].y);
    return;
  }
  const dx = new Array(n - 1), dm = new Array(n - 1), tan = new Array(n);
  for (let i = 0; i < n - 1; i++) {
    dx[i] = pts[i + 1].x - pts[i].x;
    dm[i] = dx[i] > 0 ? (pts[i + 1].y - pts[i].y) / dx[i] : 0;
  }
  tan[0] = dm[0];
  tan[n - 1] = dm[n - 2];
  for (let i = 1; i < n - 1; i++) {
    if (dm[i - 1] * dm[i] <= 0) { tan[i] = 0; continue; } // 局部极值点切线取平
    const w1 = 2 * dx[i] + dx[i - 1], w2 = dx[i] + 2 * dx[i - 1];
    tan[i] = (w1 + w2) / (w1 / dm[i - 1] + w2 / dm[i]);
  }
  for (let i = 0; i < n - 1; i++) {
    g.bezierCurveTo(
      pts[i].x + dx[i] / 3, pts[i].y + (tan[i] * dx[i]) / 3,
      pts[i + 1].x - dx[i] / 3, pts[i + 1].y - (tan[i + 1] * dx[i]) / 3,
      pts[i + 1].x, pts[i + 1].y,
    );
  }
}

function drawFrame(g, w, h, pad, yMax, unitFmt) {
  const ih = h - pad.t - pad.b, iw = w - pad.l - pad.r;
  g.strokeStyle = cssVar('--border', 'rgba(127,127,127,.25)');
  g.lineWidth = 1;
  g.font = axisFont();
  g.fillStyle = cssVar('--muted', '#8792a4');
  g.textAlign = 'right';
  for (let i = 0; i <= 2; i++) {
    const y = pad.t + (ih * i) / 2;
    g.beginPath();
    g.moveTo(pad.l, y);
    g.lineTo(pad.l + iw, y);
    g.stroke();
    g.fillText(unitFmt(yMax * (1 - i / 2)), pad.l - 6, y + 3.5);
  }
  return { iw, ih };
}

function drawXLabels(g, labels, pad, w, h) {
  const ih = h - pad.t - pad.b, iw = w - pad.l - pad.r;
  g.font = axisFont();
  g.fillStyle = cssVar('--muted', '#8792a4');
  g.textAlign = 'center';
  for (const i of xLabelIdx(labels.length, 7)) {
    g.fillText(labels[i], pad.l + (iw * i) / Math.max(1, labels.length - 1), h - 6);
  }
}

// lineChart 多序列平滑折线图。series: [{color, values}]；opts.fill 首序列画淡填充。
export function lineChart(cv, labels, series, opts = {}) {
  const h = opts.height || 190;
  const { g, w } = prepCanvas(cv, h);
  const pad = { l: 52, r: 14, t: 14, b: 24 };
  const ih = h - pad.t - pad.b, iw = w - pad.l - pad.r;
  let max = 0;
  for (const s of series) for (const v of s.values) if (v > max) max = v;
  const yMax = niceMax(max * 1.08);
  const { ih: iih } = drawFrame(g, w, h, pad, yMax, fmtAxis);
  drawXLabels(g, labels, pad, w, h);
  const X = (i) => pad.l + (iw * i) / Math.max(1, labels.length - 1);
  const Y = (v) => pad.t + iih - (v / yMax) * iih;
  series.forEach((s, si) => {
    const pts = s.values.map((v, i) => ({ x: X(i), y: Y(v) }));
    if (opts.fill && si === 0 && max > 0) {
      g.beginPath();
      smoothPath(g, pts);
      g.lineTo(pts[pts.length - 1].x, pad.t + iih);
      g.lineTo(pts[0].x, pad.t + iih);
      g.closePath();
      g.fillStyle = hexA(s.color, 0.14);
      g.fill();
    }
    g.beginPath();
    smoothPath(g, pts);
    g.strokeStyle = s.color;
    g.lineWidth = 2;
    g.lineJoin = 'round';
    g.stroke();
    const last = pts[pts.length - 1];
    g.fillStyle = s.color;
    g.beginPath();
    g.arc(last.x, last.y, 3, 0, Math.PI * 2);
    g.fill();
  });
}

// stackedBars 按日堆叠柱（消费金额按模型拆分）。series 同序堆叠。
export function stackedBars(cv, labels, series, opts = {}) {
  const h = opts.height || 200;
  const { g, w } = prepCanvas(cv, h);
  const pad = { l: 52, r: 14, t: 14, b: 24 };
  const ih = h - pad.t - pad.b, iw = w - pad.l - pad.r;
  const totals = labels.map((_, i) => series.reduce((s, ser) => s + (ser.values[i] || 0), 0));
  const yMax = niceMax(Math.max(...totals) * 1.08);
  const { ih: iih } = drawFrame(g, w, h, pad, yMax, fmtAxis);
  drawXLabels(g, labels, pad, w, h);
  const n = labels.length;
  const step = iw / Math.max(1, n);
  const bw = Math.max(4, Math.min(26, step * 0.55));
  labels.forEach((_, i) => {
    let acc = 0;
    const x = pad.l + step * i + (step - bw) / 2;
    series.forEach((s) => {
      const v = s.values[i] || 0;
      if (v <= 0) return;
      const y0 = pad.t + iih - ((acc + v) / yMax) * iih;
      const y1 = pad.t + iih - (acc / yMax) * iih;
      g.fillStyle = s.color;
      g.beginPath();
      g.roundRect(x, y0, bw, Math.max(1, y1 - y0), 2);
      g.fill();
      acc += v;
    });
  });
}

// bars 单序列柱状图（按模型 Tokens）。
export function bars(cv, labels, values, color, opts = {}) {
  const h = opts.height || 170;
  const { g, w } = prepCanvas(cv, h);
  const pad = { l: 52, r: 14, t: 14, b: 24 };
  const ih = h - pad.t - pad.b, iw = w - pad.l - pad.r;
  const yMax = niceMax(Math.max(...values, 0) * 1.08);
  const { ih: iih } = drawFrame(g, w, h, pad, yMax, fmtAxis);
  drawXLabels(g, labels, pad, w, h);
  const n = labels.length;
  const step = iw / Math.max(1, n);
  const bw = Math.max(4, Math.min(26, step * 0.55));
  values.forEach((v, i) => {
    if (v <= 0) return;
    const x = pad.l + step * i + (step - bw) / 2;
    const y = pad.t + iih - (v / yMax) * iih;
    g.fillStyle = color;
    g.beginPath();
    g.roundRect(x, y, bw, Math.max(1, pad.t + iih - y), 2);
    g.fill();
  });
}

// donut 环形图（中心总量）。items: [{value, color}]。
export function donut(cv, items, centerTop, centerSub) {
  const h = cv.clientHeight || 230;
  const { g, w } = prepCanvas(cv, h);
  const cx = w / 2, cy = h / 2;
  const R = Math.min(w, h) / 2 - 4;
  const r = R * 0.64;
  const total = items.reduce((s, it) => s + it.value, 0);
  if (total <= 0) {
    g.beginPath();
    g.arc(cx, cy, (R + r) / 2, 0, Math.PI * 2);
    g.strokeStyle = cssVar('--heat-0', '#eceef4');
    g.lineWidth = R - r;
    g.stroke();
  } else {
    let ang = -Math.PI / 2;
    const pad = items.length > 1 ? 0.028 : 0;
    items.forEach((it) => {
      const sweep = (it.value / total) * Math.PI * 2;
      if (sweep - pad > 0.001) {
        g.beginPath();
        g.arc(cx, cy, (R + r) / 2, ang + pad / 2, ang + sweep - pad / 2);
        g.strokeStyle = it.color;
        g.lineWidth = R - r;
        g.lineCap = 'butt';
        g.stroke();
      }
      ang += sweep;
    });
  }
  g.fillStyle = cssVar('--fg', '#1b1f2d');
  g.font = '600 21px ' + cssVar('--font', 'sans-serif');
  g.textAlign = 'center';
  g.fillText(centerTop, cx, cy - 1);
  g.fillStyle = cssVar('--muted', '#8792a4');
  g.font = '11.5px ' + cssVar('--font', 'sans-serif');
  g.fillText(centerSub, cx, cy + 17);
}

function hexA(hex, a) {
  const m = hex.replace('#', '');
  const n = parseInt(m.length === 3 ? m.split('').map((c) => c + c).join('') : m, 16);
  return `rgba(${(n >> 16) & 255},${(n >> 8) & 255},${n & 255},${a})`;
}

// heatmap GitHub 风格贡献热力图（DOM 网格，title 悬浮提示）。
// days: [{date:'2026-10-02', tokens, requests, credits}] 升序；mode: daily|weekly|cumulative。
// 格宽按容器宽度自适应（9–18px），月份标签按列号定位。
export function heatmap(el, days, mode) {
  const GAP = 3, WD = 23, WEEKS = 26;
  // 格宽随卡片宽度自适应（9–18px）：宽屏撑满卡片，窄屏保持可读并横向滚动。
  const avail = (el.clientWidth || 600) - WD - (WEEKS - 1) * GAP;
  const CELL = Math.max(9, Math.min(18, Math.floor(avail / WEEKS)));
  el.style.setProperty('--cell', CELL + 'px');
  const byDate = new Map(days.map((d) => [d.date, d]));
  const end = days.length ? new Date(days[days.length - 1].date + 'T12:00:00') : new Date();
  // 结束日所在的周（周一为界）为最后一列；列数=26 周（或数据覆盖更短时按实际）。
  const endCol = weekStart(end);
  const startCol = new Date(endCol.getTime() - (WEEKS - 1) * 7 * 86400000);
  const dstr = (d) => {
    const p = (x) => String(x).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
  };
  const cells = []; // {col,row,date}
  for (let wi = 0; wi < WEEKS; wi++) {
    for (let di = 0; di < 7; di++) {
      const d = new Date(startCol.getTime() + (wi * 7 + di) * 86400000);
      cells.push({ col: wi, row: di, date: dstr(d), d });
    }
  }
  // 值：daily=每日 / weekly=周合计 / cumulative=周末累计
  const weekAgg = new Map(); // mondayKey -> {tokens, requests, credits, days}
  for (const day of days) {
    const wk = dstr(weekStart(new Date(day.date + 'T12:00:00')));
    const a = weekAgg.get(wk) || { tokens: 0, requests: 0, credits: 0, days: 0 };
    a.tokens += day.tokens || 0;
    a.requests += day.requests || 0;
    a.credits += day.credits || 0;
    a.days++;
    weekAgg.set(wk, a);
  }
  let cum = 0;
  const cumByWeek = new Map();
  [...weekAgg.keys()].sort().forEach((k) => {
    cum += weekAgg.get(k).tokens;
    cumByWeek.set(k, cum);
  });
  const maxDaily = Math.max(...days.map((d) => d.tokens || 0), 0);
  const maxWeekly = Math.max(...[...weekAgg.values()].map((a) => a.tokens), 0);
  const maxCum = cum;

  const tip = (t, r, c) => `${t} tokens · ${fmtInt(r)} 请求 · ${fmtCredit(c)} 积分`;
  const level = (v, max) => (max <= 0 || v <= 0 ? 0 : Math.min(4, 1 + Math.floor((v / max) * 3.999)));

  let grid = '';
  for (const c of cells) {
    const inFuture = c.d > end;
    if (inFuture) { grid += `<i class="heat-cell blank"></i>`; continue; }
    if (mode === 'daily') {
      const day = byDate.get(c.date);
      const v = day ? day.tokens || 0 : 0;
      grid += `<i class="heat-cell l${level(v, maxDaily)}${day ? '' : ' l0'}" title="${c.date} · ${tip(fmtTokens(v), day ? day.requests : 0, day ? day.credits : 0)}"></i>`;
    } else if (mode === 'weekly') {
      if (c.row !== 0) { grid += `<i class="heat-cell blank"></i>`; continue; }
      const a = weekAgg.get(c.date);
      const v = a ? a.tokens : 0;
      grid += `<i class="heat-cell l${level(v, maxWeekly)}" title="周 ${c.date} 起 · ${tip(fmtTokens(v), a ? a.requests : 0, a ? a.credits : 0)}"></i>`;
    } else {
      if (c.row !== 0) { grid += `<i class="heat-cell blank"></i>`; continue; }
      const v = cumByWeek.get(c.date) || 0;
      grid += `<i class="heat-cell l${level(v, maxCum)}" title="截至 ${c.date}（周日）累计 · ${fmtTokens(v)} tokens"></i>`;
    }
  }
  // 月份标签：第一列即当月 1 日所在列时标月份名。
  let months = '';
  let lastMon = -1;
  for (let wi = 0; wi < WEEKS; wi++) {
    const probe = new Date(startCol.getTime() + wi * 7 * 86400000);
    if (probe.getMonth() !== lastMon) {
      lastMon = probe.getMonth();
      if (wi > 0 || probe.getDate() <= 7) {
        months += `<span style="left:${wi * (CELL + GAP)}px">${probe.getMonth() + 1}月</span>`;
      }
    }
  }
  el.innerHTML = `
    <div class="heat-scroll"><div style="width:${WD + WEEKS * (CELL + GAP)}px">
      <div class="heat-months">${months}</div>
      <div class="heat-body">
        <div class="heat-wd"><span>一</span><span></span><span>三</span><span></span><span>五</span><span></span><span></span></div>
        <div class="heat-grid">${grid}</div>
      </div>
    </div></div>`;
}

function weekStart(d) {
  const day = (d.getDay() + 6) % 7; // 周一=0
  const x = new Date(d.getTime());
  x.setHours(12, 0, 0, 0);
  x.setDate(x.getDate() - day);
  return x;
}
