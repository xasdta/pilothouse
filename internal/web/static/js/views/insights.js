// Insights (F6; workstream I): usage and estimated cost over 7/30/90
// days, by agent/model/repo, what the rules did, and today's budget.
// Charts are inline SVG built here (no chart library), each with a
// table twin, keyboard focus and a tooltip that never gates a value.
// Colours are the --viz-* tokens in views.css (validated against both
// surfaces; dark mode uses its own darker steps).

import { h, agentName, agentClass, fill, add } from '../dom.js';
import { api } from '../api.js';
import { ico, fmtTokens, fmtUSD, fmtDuration, totalTokens } from './session.js';

const SVGNS = 'http://www.w3.org/2000/svg';
const RANGES = [7, 30, 90];
const RANGE_KEY = 'pilothouse.insights.days';
const METRIC_KEY = 'pilothouse.insights.metric';
const whole = new Intl.NumberFormat();
const MAX_GROUPS = 6; // past this, the tail folds into "Other"

function svg(tag, attrs, ...kids) {
  const el = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) if (v != null && v !== false) el.setAttribute(k, String(v));
  add(el, ...kids);
  return el;
}

const dayDate = (d) => new Date(d + 'T12:00:00');
const fmtDay = (d, opts) => dayDate(d).toLocaleDateString([], opts);
const fmtCount = (n) => (n >= 10000 ? fmtTokens(n) : whole.format(n || 0));
function fmtMinutes(m) {
  m = Number(m) || 0;
  if (m < 1) return m > 0 ? '<1 min' : '0 min';
  if (m < 90) return Math.round(m) + ' min';
  return (m / 60).toFixed(1).replace(/\.0$/, '') + ' h';
}

/** A clean axis maximum and step for 3–4 gridlines. */
export function niceScale(max, ticks = 4) {
  if (!(max > 0)) return { max: 1, step: 1 };
  const raw = max / ticks;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw) || 10 * mag;
  return { max: Math.ceil(max / step - 1e-9) * step, step };
}

export function insightsView() {
  const root = h('div', { class: 'view insights' });
  let days = Number(localStorage.getItem(RANGE_KEY)) || 7;
  if (!RANGES.includes(days)) days = 7;
  let metric = localStorage.getItem(METRIC_KEY) === 'tokens' ? 'tokens' : 'cost';
  let by = 'agent';
  let showTable = false;
  const cache = new Map();
  let data = null;
  let err = null;
  let loading = false;
  let destroyed = false;
  let seq = 0;

  const rangeSeg = radioSeg('Range', 'range', RANGES.map((d) => [String(d), `${d} days`]), String(days), (v) => {
    days = Number(v);
    try { localStorage.setItem(RANGE_KEY, v); } catch { /* not remembered */ }
    load();
  });
  const metricSeg = radioSeg('Measure', 'metric', [['cost', 'Cost'], ['tokens', 'Tokens']], metric, (v) => {
    metric = v;
    try { localStorage.setItem(METRIC_KEY, v); } catch { /* not remembered */ }
    render();
  });
  const body = h('div', { class: 'ins-body' });
  add(root,
    h('div', { class: 'page-head' },
      h('h1', { class: 'page-title' }, 'Insights'),
      h('p', { class: 'page-sub' }, 'Usage, estimated cost and what your rules did')),
    h('div', { class: 'ins-filters' }, rangeSeg.el, metricSeg.el),
    body);

  async function load() {
    const my = ++seq;
    if (cache.has(days)) {
      data = cache.get(days);
      err = null;
      render();
    }
    loading = true;
    body.classList.toggle('refreshing', !!data);
    try {
      const d = await api('GET', `/api/insights?days=${days}`);
      if (destroyed || my !== seq) return;
      cache.set(days, d);
      data = d;
      err = null;
    } catch (e) {
      if (destroyed || my !== seq) return;
      err = e;
      if (!cache.has(days)) data = null;
    }
    loading = false;
    body.classList.remove('refreshing');
    render();
  }

  function render() {
    for (const o of observers.splice(0)) o.disconnect(); // the old chart is going away
    if (!data) {
      if (err && err.status === 501) {
        fill(body, h('div', { class: 'empty' },
          h('div', { class: 'halo' }, ico('coins', 'lg')),
          h('h2', null, 'Insights need a newer Pilothouse'),
          h('p', null, 'Update Pilothouse on your computer to see tokens, cost and what your rules did.')));
      } else if (err) {
        fill(body, h('div', { class: 'empty' }, h('h2', null, 'Couldn’t load insights'), h('p', null, err.message),
          h('button', { class: 'btn secondary compact', type: 'button', onclick: load }, 'Try again')));
      } else {
        fill(body, h('div', { class: 'ins-skel', 'aria-busy': 'true' }, h('span', { class: 'sr-only' }, 'Loading insights…'),
          h('div', { class: 'card skel-block' }), h('div', { class: 'card skel-block tall' })));
      }
      return;
    }
    const d = data;
    const list = d.days || [];
    const tokens = totalTokens(d.total);
    const sessions = list.reduce((n, x) => n + (x.sessions || 0), 0);
    const empty = !tokens && !(d.total && d.total.cost_usd) && !(d.rules && (d.rules.auto_allowed || d.rules.asked || d.rules.blocked));
    const range = days === 1 ? 'today' : `the last ${days} days`;
    if (empty) {
      fill(body, h('div', { class: 'empty' },
        h('div', { class: 'halo' }, ico('coins', 'lg')),
        h('h2', null, 'Nothing recorded yet'),
        h('p', null, `No agent usage or rule decisions in ${range}. Insights fill in as your agents work.`)));
      return;
    }
    fill(body,
      h('section', { class: 'card ins-hero', 'aria-label': 'Totals' },
        h('div', { class: 'ins-hero-main' },
          h('p', { class: 'ins-hero-label' }, metric === 'cost' ? `Estimated cost · ${range}` : `Tokens · ${range}`),
          h('p', { class: 'ins-hero-value' }, metric === 'cost' ? fmtUSD(d.total.cost_usd) : fmtTokens(tokens)),
          h('p', { class: 'ins-hero-sub' },
            metric === 'cost' ? `${fmtTokens(tokens)} tokens` : `≈ ${fmtUSD(d.total.cost_usd)} estimated`,
            ` · ${whole.format(sessions)} ${sessions === 1 ? 'session' : 'sessions'}`)),
        budgetEl(d.budget)),
      dailyChart(list, d),
      breakdown(d),
      h('section', { class: 'section', 'aria-labelledby': 'rules-h' },
        h('h2', { class: 'section-head', id: 'rules-h' }, 'What the rules did'),
        ruleTiles(d.rules || {}, list)),
      topLists(d.rules || {}),
      h('p', { class: 'ins-note' }, ico('info', 'sm'), 'Costs are estimates from list prices and leave out models Pilothouse doesn’t know.'));
  }

  // ---- Budget ----
  function budgetEl(b) {
    if (!b || !(b.daily_usd > 0)) {
      return h('div', { class: 'ins-budget none' },
        h('p', null, h('b', null, 'No daily budget'), h('span', null, 'Set usage.daily_budget_usd in config.yaml to get alerts at 80 % and 100 %.')));
    }
    const pct = Math.max(0, b.percent || (b.spent_usd / b.daily_usd) * 100);
    const level = pct >= 100 ? 'over' : pct >= 80 ? 'near' : 'ok';
    const label = { ok: 'On track', near: 'Close to budget', over: 'Over budget' }[level];
    const fillEl = h('div', { class: 'meter-fill' });
    fillEl.style.width = Math.min(100, pct).toFixed(1) + '%'; // CSSOM, allowed by the CSP
    return h('div', { class: 'ins-budget ' + level },
      h('div', { class: 'ins-budget-top' },
        h('span', { class: 'ins-budget-t' }, 'Today'),
        h('span', { class: 'ins-budget-status' }, ico(level === 'ok' ? 'check' : 'alert', 'sm'), label)),
      h('p', { class: 'ins-budget-v' }, h('b', null, fmtUSD(b.spent_usd)), ` of ${fmtUSD(b.daily_usd)}`, h('span', { class: 'pct' }, `${Math.round(pct)} %`)),
      h('div', {
        class: 'meter', role: 'meter', 'aria-valuemin': '0', 'aria-valuemax': String(b.daily_usd), 'aria-valuenow': String(Math.min(b.spent_usd, b.daily_usd * 2)),
        'aria-label': `Today’s spend: ${fmtUSD(b.spent_usd)} of a ${fmtUSD(b.daily_usd)} budget (${Math.round(pct)} %)`,
      }, fillEl, h('i', { class: 'meter-mark' })),
      b.burn_usd_per_hour > 0 ? h('p', { class: 'ins-burn' }, ico('clock', 'sm'), `${fmtUSD(b.burn_usd_per_hour)} in the last hour`) : null);
  }

  // ---- Daily chart ----
  function dailyChart(list, d) {
    const val = (x) => (metric === 'cost' ? (x.usage && x.usage.cost_usd) || 0 : totalTokens(x.usage));
    const fmt = metric === 'cost' ? fmtUSD : fmtTokens;
    const title = metric === 'cost' ? 'Estimated cost per day' : 'Tokens per day';
    const wrap = h('div', { class: 'chart' });
    const tip = h('div', { class: 'chart-tip', role: 'status', 'aria-live': 'polite', hidden: true });
    const tableBtn = h('button', { class: 'linkish', type: 'button', 'aria-expanded': String(showTable), 'aria-controls': 'daily-table' }, showTable ? 'Hide table' : 'Show as table');
    const table = h('div', { class: 'chart-table', id: 'daily-table', hidden: !showTable }, dailyTable(list));
    tableBtn.addEventListener('click', () => {
      showTable = !showTable;
      table.hidden = !showTable;
      tableBtn.textContent = showTable ? 'Hide table' : 'Show as table';
      tableBtn.setAttribute('aria-expanded', String(showTable));
    });
    const card = h('section', { class: 'card ins-card', 'aria-labelledby': 'daily-h' },
      h('div', { class: 'ins-card-head' }, h('h2', { id: 'daily-h' }, title), tableBtn),
      h('div', { class: 'chart-box' }, wrap, tip),
      table);
    let active = -1;
    const draw = () => {
      const width = Math.max(260, Math.round(wrap.clientWidth || 320));
      fill(wrap, columns(list, val, fmt, width, title, d));
      if (active >= 0) show(active);
    };
    function show(i) {
      active = i;
      const x = list[i];
      if (!x) { tip.hidden = true; return; }
      const bars = wrap.querySelectorAll('.bar');
      bars.forEach((b, j) => b.classList.toggle('on', j === i));
      wrap.classList.add('has-on');
      fill(tip,
        h('b', null, fmt(val(x))),
        h('span', null, fmtDay(x.date, { weekday: 'short', month: 'short', day: 'numeric' })),
        h('span', null, metric === 'cost' ? `${fmtTokens(totalTokens(x.usage))} tokens` : `≈ ${fmtUSD(x.usage && x.usage.cost_usd)}`, ` · ${x.sessions || 0} sessions`));
      tip.hidden = false;
      const svgEl = wrap.querySelector('svg');
      const band = Number(svgEl.dataset.band);
      const left = Number(svgEl.dataset.left);
      const cx = left + band * (i + 0.5);
      const w = tip.offsetWidth || 140;
      tip.style.left = Math.max(0, Math.min(wrap.clientWidth - w, cx - w / 2)) + 'px';
      // Above the bar's cap; it may float over the card title for tall bars.
      const bar = bars[i];
      const capY = bar ? bar.getBoundingClientRect().top - wrap.getBoundingClientRect().top : 0;
      tip.style.top = Math.max(-48, capY - tip.offsetHeight - 10) + 'px';
    }
    function hide() {
      active = -1;
      tip.hidden = true;
      wrap.classList.remove('has-on');
      wrap.querySelectorAll('.bar.on').forEach((b) => b.classList.remove('on'));
    }
    wrap.addEventListener('pointermove', (e) => {
      const svgEl = wrap.querySelector('svg');
      if (!svgEl) return;
      const r = svgEl.getBoundingClientRect();
      const i = Math.floor((e.clientX - r.left - Number(svgEl.dataset.left)) / Number(svgEl.dataset.band));
      if (i >= 0 && i < list.length) show(i); else hide();
    });
    wrap.addEventListener('pointerleave', hide);
    wrap.addEventListener('focusin', () => show(active >= 0 ? active : list.length - 1));
    wrap.addEventListener('focusout', hide);
    wrap.addEventListener('keydown', (e) => {
      if (e.key === 'ArrowRight' || e.key === 'ArrowLeft' || e.key === 'Home' || e.key === 'End') {
        e.preventDefault();
        const n = e.key === 'Home' ? 0 : e.key === 'End' ? list.length - 1 : Math.max(0, Math.min(list.length - 1, (active < 0 ? list.length - 1 : active) + (e.key === 'ArrowRight' ? 1 : -1)));
        show(n);
      } else if (e.key === 'Escape') hide();
    });
    // Size to the card once it is in the page, and again when it resizes.
    requestAnimationFrame(draw);
    const ro = new ResizeObserver(() => draw());
    ro.observe(wrap);
    observers.push(ro);
    return card;
  }

  function columns(list, val, fmt, width, title, d) {
    const H = 176;
    const top = 18;
    const bottom = 24;
    const values = list.map(val);
    const { max, step } = niceScale(Math.max(...values, 0));
    const ticks = [];
    for (let v = 0; v <= max + 1e-9; v += step) ticks.push(v);
    const labelW = Math.max(...ticks.map((t) => fmt(t).length)) * 7 + 10;
    const left = labelW;
    const plotW = width - left - 4;
    const plotH = H - top - bottom;
    const band = plotW / Math.max(1, list.length);
    const barW = Math.max(2, Math.min(24, band - Math.max(1, Math.min(8, band * 0.34))));
    const y = (v) => top + plotH - (v / max) * plotH;
    const peak = values.indexOf(Math.max(...values));
    const totalV = values.reduce((a, b) => a + b, 0);
    const desc = list.length
      ? `${list.length} days from ${fmtDay(list[0].date, { month: 'short', day: 'numeric' })} to ${fmtDay(list[list.length - 1].date, { month: 'short', day: 'numeric' })}. Total ${fmt(totalV)}. Highest ${fmt(values[peak])} on ${fmtDay(list[peak].date, { weekday: 'long', month: 'short', day: 'numeric' })}.`
      : 'No days.';
    const s = svg('svg', {
      class: 'cols', viewBox: `0 0 ${width} ${H}`, width, height: H, role: 'img', 'aria-labelledby': 'daily-st daily-sd',
      tabindex: '0', 'data-band': band.toFixed(3), 'data-left': left,
    },
    svg('title', { id: 'daily-st' }, title),
    svg('desc', { id: 'daily-sd' }, desc + ' Use the arrow keys to read each day.'));
    for (const t of ticks) {
      add(s, svg('line', { class: t === 0 ? 'axis' : 'grid', x1: left, x2: width - 4, y1: y(t).toFixed(1), y2: y(t).toFixed(1) }));
      add(s, svg('text', { class: 'tick', x: left - 8, y: (y(t) + 4).toFixed(1), 'text-anchor': 'end' }, fmt(t)));
    }
    const every = list.length <= 7 ? 1 : list.length <= 31 ? 7 : 15;
    list.forEach((x, i) => {
      const v = values[i];
      const bx = left + band * i + (band - barW) / 2;
      if (v > 0) {
        const by = y(v);
        const r = Math.min(4, barW / 2, (top + plotH - by));
        const base = top + plotH;
        // Rounded data end, square at the baseline.
        add(s, svg('path', {
          class: 'bar' + (i === peak ? ' peak' : ''),
          d: `M${bx.toFixed(2)},${base}V${(by + r).toFixed(2)}Q${bx.toFixed(2)},${by.toFixed(2)} ${(bx + r).toFixed(2)},${by.toFixed(2)}H${(bx + barW - r).toFixed(2)}Q${(bx + barW).toFixed(2)},${by.toFixed(2)} ${(bx + barW).toFixed(2)},${(by + r).toFixed(2)}V${base}Z`,
        }));
      } else {
        add(s, svg('rect', { class: 'bar zero', x: bx.toFixed(2), y: top + plotH - 1, width: barW.toFixed(2), height: 1 }));
      }
      if ((list.length - 1 - i) % every === 0) {
        const label = list.length <= 7 ? fmtDay(x.date, { weekday: 'short' }) : fmtDay(x.date, { month: 'short', day: 'numeric' });
        add(s, svg('text', { class: 'tick', x: (left + band * (i + 0.5)).toFixed(1), y: H - 6, 'text-anchor': 'middle' }, label));
      }
    });
    // One direct label: the peak, on its cap.
    if (values[peak] > 0) {
      const px = left + band * (peak + 0.5);
      const anchor = px < left + 30 ? 'start' : px > width - 30 ? 'end' : 'middle';
      add(s, svg('text', { class: 'cap', x: px.toFixed(1), y: (y(values[peak]) - 6).toFixed(1), 'text-anchor': anchor }, fmt(values[peak])));
    }
    return s;
  }

  function dailyTable(list) {
    return h('table', { class: 'ins-table' },
      h('caption', { class: 'sr-only' }, 'Usage and decisions per day'),
      h('thead', null, h('tr', null,
        ['Day', 'Est. cost', 'Tokens', 'Sessions', 'Auto-allowed', 'Asked', 'Blocked'].map((c, i) => h('th', { scope: 'col', class: i ? 'num' : '' }, c)))),
      h('tbody', null, list.slice().reverse().map((x) => h('tr', null,
        h('th', { scope: 'row' }, fmtDay(x.date, { weekday: 'short', month: 'short', day: 'numeric' })),
        h('td', { class: 'num' }, fmtUSD(x.usage && x.usage.cost_usd)),
        h('td', { class: 'num' }, fmtTokens(totalTokens(x.usage))),
        h('td', { class: 'num' }, whole.format(x.sessions || 0)),
        h('td', { class: 'num' }, whole.format(x.auto_allowed || 0)),
        h('td', { class: 'num' }, whole.format(x.asked || 0)),
        h('td', { class: 'num' }, whole.format(x.blocked || 0))))));
  }

  // ---- Breakdown ----
  function breakdown(d) {
    const groups = { agent: d.by_agent || [], model: d.by_model || [], repo: d.by_repo || [] };
    const val = (g) => (metric === 'cost' ? g.usage.cost_usd || 0 : totalTokens(g.usage));
    const fmt = metric === 'cost' ? fmtUSD : fmtTokens;
    const listEl = h('div', { class: 'hbars' });
    const seg = radioSeg('Group by', 'by', [['agent', 'Agent'], ['model', 'Model'], ['repo', 'Repo']], by, (v) => { by = v; draw(); }, 'small');
    function draw() {
      let rows = (groups[by] || []).filter((g) => g && g.usage).slice().sort((a, b) => val(b) - val(a));
      if (rows.length > MAX_GROUPS) {
        const tail = rows.slice(MAX_GROUPS - 1);
        const other = { key: '', other: tail.length, usage: { cost_usd: 0, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0 }, sessions: 0 };
        for (const g of tail) {
          for (const k of Object.keys(other.usage)) other.usage[k] += g.usage[k] || 0;
          other.sessions += g.sessions || 0;
        }
        rows = [...rows.slice(0, MAX_GROUPS - 1), other];
      }
      const max = Math.max(...rows.map(val), 0);
      if (!rows.length || !max) {
        fill(listEl, h('p', { class: 'quiet' }, metric === 'cost' ? 'No cost recorded for these.' : 'No tokens recorded.'));
        return;
      }
      fill(listEl, rows.map((g) => {
        const v = val(g);
        const bar = h('i', { class: 'hbar-fill' });
        bar.style.width = Math.max(v > 0 ? 1.5 : 0, (v / max) * 100).toFixed(2) + '%';
        const name = g.other ? `Other (${g.other})` : by === 'agent' ? agentName(g.key) : g.key || 'Unknown';
        return h('div', { class: 'hbar' },
          h('span', { class: 'hbar-label' },
            by === 'agent' && !g.other ? h('span', { class: 'agent-dot ' + agentClass(g.key) }) : null,
            h('span', { class: by === 'model' ? 'mono' : '' }, name)),
          h('span', { class: 'hbar-track' }, bar),
          h('span', { class: 'hbar-v' }, h('b', null, fmt(v)), h('span', null, `${g.sessions || 0} ${g.sessions === 1 ? 'session' : 'sessions'}`)));
      }));
    }
    draw();
    return h('section', { class: 'card ins-card', 'aria-labelledby': 'by-h' },
      h('div', { class: 'ins-card-head' }, h('h2', { id: 'by-h' }, metric === 'cost' ? 'Where the cost went' : 'Where the tokens went'), seg.el),
      listEl);
  }

  // ---- Rules ----
  function ruleTiles(r, list) {
    const series = (k) => list.map((x) => x[k] || 0);
    const tiles = [
      { label: 'Auto-allowed', value: fmtCount(r.auto_allowed), sub: 'by rules and session grants', spark: series('auto_allowed'), ic: 'check', cls: 'good' },
      { label: 'Asked you', value: fmtCount(r.asked), sub: `${whole.format(r.answered_by_device || 0)} on a device · ${whole.format(r.answered_at_terminal || 0)} at the computer${r.expired ? ` · ${r.expired} timed out` : ''}`, spark: series('asked'), ic: 'bell' },
      { label: 'Blocked', value: fmtCount(r.blocked), sub: 'denied by rules', spark: series('blocked'), ic: 'x', cls: 'bad' },
      { label: 'Allowed for session', value: fmtCount(r.granted), sub: 'answers that covered a whole session', ic: 'shield' },
      { label: 'Median answer time', value: r.median_answer_secs ? fmtDuration(r.median_answer_secs) : '—', sub: 'from asking to your answer', ic: 'clock' },
      { label: 'Agents waited', value: fmtMinutes(r.blocked_minutes), sub: 'blocked, waiting on a person', ic: 'hourglass' },
    ];
    return h('div', { class: 'tiles' }, tiles.map((t) => h('div', { class: 'tile ' + (t.cls || '') },
      h('p', { class: 'tile-label' }, ico(t.ic, 'sm'), t.label),
      h('p', { class: 'tile-value' }, t.value),
      t.spark && t.spark.length > 1 ? sparkline(t.spark, `${t.label} per day`) : null,
      h('p', { class: 'tile-sub' }, t.sub))));
  }

  function topLists(r) {
    const block = (title, rows, cls, ic) => {
      rows = (rows || []).filter((x) => x && x.count > 0);
      if (!rows.length) return null;
      const max = Math.max(...rows.map((x) => x.count));
      return h('section', { class: 'card ins-card ' + cls, 'aria-label': title },
        h('div', { class: 'ins-card-head' }, h('h2', null, ico(ic, 'sm'), title)),
        h('ol', { class: 'hbars ranked' }, rows.slice(0, 8).map((x) => {
          const bar = h('i', { class: 'hbar-fill' });
          bar.style.width = ((x.count / max) * 100).toFixed(2) + '%';
          return h('li', { class: 'hbar' },
            h('span', { class: 'hbar-label' }, h('span', { class: 'mono' }, x.key)),
            h('span', { class: 'hbar-track' }, bar),
            h('span', { class: 'hbar-v' }, h('b', null, whole.format(x.count))));
        })));
    };
    const a = block('Rules that allowed the most', r.top_rules, 'top-allow', 'shield');
    const b = block('Rules that blocked the most', r.top_blocked, 'top-block', 'x');
    return a || b ? h('div', { class: 'ins-pair' }, a, b) : null;
  }

  const observers = [];
  load();
  render();
  return {
    el: root,
    update: () => {},
    destroy() { destroyed = true; for (const o of observers) o.disconnect(); },
  };
}

/** A sparkline: de-emphasised line, the latest point in the accent. */
function sparkline(values, label) {
  const W = 120;
  const H = 28;
  const max = Math.max(...values, 1);
  const pts = values.map((v, i) => [(i / (values.length - 1)) * (W - 6) + 3, H - 4 - (v / max) * (H - 8)]);
  const d = pts.map((p, i) => (i ? 'L' : 'M') + p[0].toFixed(1) + ',' + p[1].toFixed(1)).join('');
  const last = pts[pts.length - 1];
  // Stretched to the tile's width; non-scaling strokes keep the line 2 px and
  // the end dot round (a zero-length round-capped stroke, with a surface ring).
  const dot = `M${last[0].toFixed(1)},${last[1].toFixed(1)}h0`;
  return svg('svg', { class: 'spark', viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: 'none', role: 'img', 'aria-label': `${label}: from ${values[0]} to ${values[values.length - 1]}` },
    svg('path', { class: 'spark-line', d, 'vector-effect': 'non-scaling-stroke' }),
    svg('path', { class: 'spark-ring', d: dot, 'vector-effect': 'non-scaling-stroke' }),
    svg('path', { class: 'spark-dot', d: dot, 'vector-effect': 'non-scaling-stroke' }));
}

let segN = 0;
/** A segmented radio group (native radios: arrow keys work). */
function radioSeg(label, name, options, value, onChange, size = '') {
  const id = 'seg-' + name + '-' + (++segN);
  const el = h('div', { class: 'seg-v2 ' + size, role: 'radiogroup', 'aria-label': label },
    options.map(([v, text]) => {
      const input = h('input', { type: 'radio', name: id, id: `${id}-${v}`, value: v, class: 'sr-only' });
      input.checked = v === value;
      input.addEventListener('change', () => { if (input.checked) onChange(v); });
      return h('label', { for: `${id}-${v}` }, input, h('span', null, text));
    }));
  return { el };
}
