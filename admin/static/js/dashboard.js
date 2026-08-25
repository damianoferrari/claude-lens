import { pad, esc, fmtTokens, fmtCost, fmtCountdown, fmtTime, addCost, makeAbortable, initNav, progressBar, fmtSessionId, fmtActivePeriod, computePagination, renderPaginationControls, wirePaginationNav, tokensTooltip, costTooltip } from './app.js';

'use strict';

(() => {
  const RANGES = ['today', 'this-week', 'this-month', 'last-7-days', 'last-15-days', 'last-30-days', 'last-60-days'];
  const DEFAULT_RANGE = 'this-week';

  const params = new URLSearchParams(window.location.search);
  let range = params.get('range');
  if (!RANGES.includes(range)) range = DEFAULT_RANGE;

  const select = document.getElementById('predefined-date-filter');
  if (select) select.value = range;

  function setText(id, text) {
    const el = document.getElementById(id);
    if (el) el.textContent = text;
  }

  function dateStr(d) {
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  }

  /**
   * Resolves a range preset to its lower bound; the upper bound is always
   * "now".
   * @param {string} rangeKey - One of RANGES.
   * @param {Date} now - Reference time.
   * @returns {Date} Start of the range.
   */
  function rangeFrom(rangeKey, now) {
    const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate());
    switch (rangeKey) {
      case 'today': return midnight;
      case 'this-week': { const d = new Date(midnight); d.setDate(d.getDate() - d.getDay()); return d; }
      case 'this-month': return new Date(now.getFullYear(), now.getMonth(), 1);
      case 'last-7-days': { const d = new Date(midnight); d.setDate(d.getDate() - 6); return d; }
      case 'last-15-days': { const d = new Date(midnight); d.setDate(d.getDate() - 14); return d; }
      case 'last-30-days': { const d = new Date(midnight); d.setDate(d.getDate() - 29); return d; }
      case 'last-60-days': { const d = new Date(midnight); d.setDate(d.getDate() - 59); return d; }
      default: return rangeFrom(DEFAULT_RANGE, now);
    }
  }

  function updateDateLabels() {
    const now = new Date();
    setText('date-from-value', dateStr(rangeFrom(range, now)));
    setText('date-to-value', dateStr(now));
  }
  updateDateLabels();

  // ── Stats cards ─────────────────────────────────────────────────────────
  function renderTotals(totals) {
    if (!totals) return;
    const inputTokens = totals.total_input_tokens ?? 0;
    const outputTokens = totals.total_output_tokens ?? 0;
    const cacheTokens = (totals.total_cache_creation_tokens ?? 0) + (totals.total_cache_read_tokens ?? 0);
    setText('total-input-tokens', fmtTokens(inputTokens));
    setText('total-input-cost', fmtCost(totals.total_input_cost));
    setText('total-output-tokens', fmtTokens(outputTokens));
    setText('total-output-cost', fmtCost(totals.total_output_cost));
    setText('total-cache-tokens', fmtTokens(cacheTokens));
    setText('total-cache-cost', fmtCost(addCost(totals.total_cache_creation_cost, totals.total_cache_read_cost)));
    setText('total-tokens', fmtTokens(inputTokens + outputTokens + cacheTokens));
    setText('total-cost', fmtCost(totals.total_cost));
  }

  // ── Limiters ────────────────────────────────────────────────────────────
  // Actual limiter data always comes from GET /api/limiters (SSE only
  // signals "something changed"); kept here so both the global-limiters
  // section and the per-session table column can render from the same
  // already-fetched data without duplicate requests.
  let globalLimiters = [];
  let limitersBySession = new Map();

  function limiterCard(l) {
    return `<div class="p-4 bg-white rounded-lg border border-gray-200">
      ${progressBar(l, 'h-2')}
      <div class="flex justify-between items-center mt-4">
        <span class="text-xs text-gray-400">${esc(l.within_active_period && l.is_active ? fmtCountdown(l.next_refresh_at) : 'currently inactive')}</span>
        <span class="text-xs">${fmtActivePeriod(l)}</span>
      </div>
      </div>`;
  }

  function renderGlobalLimiters() {
    const container = document.getElementById('global-limiters');
    if (!container) return;
    container.innerHTML = globalLimiters.length
      ? globalLimiters.map(limiterCard).join('')
      : '<p class="text-gray-400 text-sm">No global limiters configured.</p>';
  }

  const refreshLimiters = makeAbortable(async (signal) => {
    const res = await fetch('/api/limiters', { signal });
    if (!res.ok) return;
    const limiters = await res.json();
    globalLimiters = limiters.filter((l) => !l.session_id);
    limitersBySession = new Map(limiters.filter((l) => l.session_id).map((l) => [l.session_id, l]));
    renderGlobalLimiters();
    renderSessionRows();
  });

  // ── Per-session table ─────────────────────────────────────────────────────
  const SESSION_PAGE_SIZES = [25, 50, 100, 200];
  const DEFAULT_SESSION_PAGE_SIZE = 50;
  let lastSessionRows = [];
  let sessionPage = 1;
  let sessionPageSize = DEFAULT_SESSION_PAGE_SIZE;
  let sessionTotal = 0;

  function sessionLimiterCell(session) {
    const l = limitersBySession.get(session.session_id);
    if (!l) return '<span class="text-gray-300">—</span>';
    return `${progressBar(l)}<p class="text-xs text-gray-400 mt-1">${esc(fmtCountdown(l.within_active_period && l.is_active ? l.next_refresh_at : null))}</p>`;
  }

  function buildSessionRow(session) {
    const cacheTok = (session.total_cache_creation_tokens ?? 0) + (session.total_cache_read_tokens ?? 0);
    const inputTok = session.total_input_tokens ?? 0;
    const outputTok = session.total_output_tokens ?? 0;
    const totalTok = inputTok + outputTok + cacheTok;
    const cost = session.total_cost ?? 0;
    const costStr = cost > 0 ? fmtCost(cost) : '—';
    const exchangeCount = session.exchange_count || 0;
    const avgTok = exchangeCount ? totalTok / exchangeCount : 0;
    const avgCostStr = cost > 0 && exchangeCount ? fmtCost(cost / exchangeCount) : '—';
    const tokensRow = {
      input_tokens: inputTok,
      output_tokens: outputTok,
      cache_creation_tokens: session.total_cache_creation_tokens,
      cache_read_tokens: session.total_cache_read_tokens,
    };
    const costsRow = {
      input_cost: session.total_input_cost,
      output_cost: session.total_output_cost,
      cache_creation_cost: session.total_cache_creation_cost,
      cache_read_cost: session.total_cache_read_cost,
    };
    const sessionQuery = 'session = ' + JSON.stringify(session.session_id);
    const nameHtml = `<a href="/exchanges?q=${encodeURIComponent(sessionQuery)}" class="text-emerald-600 hover:underline font-medium">${esc(session.session_name || fmtSessionId(session.session_id, 24))}</a>${session.session_name ? `<span class="block text-xs text-gray-400 font-mono">${esc(fmtSessionId(session.session_id, 24))}</span>` : ''}`;
    return `<tr class="hover:bg-gray-50">
      <td class="px-4 py-2">${nameHtml}</td>
      <td class="px-4 py-2 text-right text-gray-700">${session.exchange_count}</td>
      <td class="px-4 py-2 text-gray-700">${esc(session.model || '—')}</td>
      <td class="px-4 py-2 text-right text-gray-700" data-tip="${esc(tokensTooltip(tokensRow))}">
        ${fmtTokens(totalTok)}
        <p class="text-xs text-gray-400">avg ${fmtTokens(avgTok)}</p>
      </td>
      <td class="px-4 py-2 text-right text-gray-700" data-tip="${esc(costTooltip(costsRow))}">
        ${costStr}
        <p class="text-xs text-gray-400">avg ${avgCostStr}</p>
      </td>
      <td class="px-4 py-2 text-gray-400 whitespace-nowrap">${fmtTime(session.last_updated)}</td>
      <td class="px-4 py-2 whitespace-nowrap text-right w-36">${sessionLimiterCell(session)}</td>
      </tr>`;
  }

  function renderSessionRows() {
    const tbody = document.getElementById('session-stats-tbody');
    if (!tbody) return;
    tbody.innerHTML = lastSessionRows.length
      ? lastSessionRows.map(buildSessionRow).join('')
      : '<tr><td colspan="7" class="px-4 py-8 text-center text-gray-400">No sessions yet.</td></tr>';
  }

  function renderSessionPagination() {
    const pagination = computePagination(sessionPage, sessionPageSize, sessionTotal);
    sessionPage = pagination.page;
    renderPaginationControls('session-pagination-controls', { ...pagination, total: sessionTotal, pageSize: sessionPageSize }, SESSION_PAGE_SIZES, (page, size) => {
      sessionPage = page;
      sessionPageSize = size;
      refreshSessionStats();
    });
  }

  wirePaginationNav('session-pagination-controls', (page) => {
    sessionPage = page;
    refreshSessionStats();
  });

  /**
   * Merges a since_id delta into the currently-loaded page of session rows
   * and refreshes the total/pagination controls to match. On page 1 (sorted
   * most-recently-active first), the changed sessions are always now the
   * most recent, so they're moved to the top and the page is re-trimmed to
   * size — reproducing what a full page-1 refetch would show. On any other
   * page, changed sessions are only patched in place when already visible on
   * that page; a session that moved onto/off of this page stays as-is until
   * the user navigates, the same tolerance the exchanges page already
   * accepts for its own non-front pages.
   * @param {object[]} changedRows - Session stat rows from GetSessionStatsSince, most-recently-active first.
   * @param {number} total - Current distinct-session count.
   */
  function applySessionDelta(changedRows, total) {
    sessionTotal = total;
    if (!changedRows.length) {
      renderSessionPagination();
      return;
    }
    const changedIds = new Set(changedRows.map((r) => r.session_id));
    if (sessionPage === 1) {
      lastSessionRows = [...changedRows, ...lastSessionRows.filter((r) => !changedIds.has(r.session_id))].slice(0, sessionPageSize);
    } else {
      const byId = new Map(changedRows.map((r) => [r.session_id, r]));
      lastSessionRows = lastSessionRows.map((r) => byId.get(r.session_id) ?? r);
    }
    renderSessionRows();
    renderSessionPagination();
  }

  // Shared abortable task for both the full page fetch and the since_id
  // delta, so a fresh call of either kind cancels a still in-flight call of
  // the other instead of letting a slower, superseded response win.
  const refreshSessionStatsTask = makeAbortable(async (signal, sinceID) => {
    const searchParams = new URLSearchParams();
    if (sinceID != null) {
      searchParams.set('since_id', sinceID);
    } else {
      searchParams.set('limit', sessionPageSize);
      searchParams.set('offset', (sessionPage - 1) * sessionPageSize);
    }
    const res = await fetch('/api/session-stats?' + searchParams.toString(), { signal });
    if (!res.ok) return;
    const data = await res.json();
    if (sinceID != null) {
      applySessionDelta(data.rows, data.total);
    } else {
      lastSessionRows = data.rows;
      sessionTotal = data.total;
      renderSessionRows();
      renderSessionPagination();
    }
  });
  const refreshSessionStats = () => refreshSessionStatsTask();
  const refreshSessionStatsSince = (sinceID) => refreshSessionStatsTask(sinceID);

  // ── Heatmap ─────────────────────────────────────────────────────────────
  let dailyCosts = [];

  function dayStr(date) {
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
  }

  /**
   * Buckets a daily cost into one of 5 heatmap levels using thresholds
   * relative to the max cost across the visible range.
   * @param {number} cost - This day's cost.
   * @param {number} maxCost - Maximum daily cost across the visible range.
   * @param {number} lowThreshold - Upper bound of level 1.
   * @param {number} midThreshold - Upper bound of level 2.
   * @param {number} highThreshold - Upper bound of level 3.
   * @returns {number} Heatmap level from 0 (no activity) to 4 (highest).
   */
  function getLevel(cost, maxCost, lowThreshold, midThreshold, highThreshold) {
    if (!cost || cost === 0 || maxCost === 0) return 0;
    if (cost < lowThreshold) return 1;
    if (cost < midThreshold) return 2;
    if (cost < highThreshold) return 3;
    return 4;
  }

  /**
   * Adaptive precision up to 2 decimals, then 0 decimals for larger values.
   * @param {number} cost - Cost to format.
   * @returns {string} Formatted cost (e.g. "~$0.00" for sub-cent, "" for zero).
   */
  function fmtCell(cost) {
    if (!cost || cost === 0) return '';
    if (cost < 0.01) return `~$${cost.toFixed(2)}`;
    if (cost < 10) return `$${cost.toFixed(2)}`;
    if (cost < 100) return `$${cost.toFixed(1)}`;
    if (cost > 999) return `(╯°□°)╯`;
    return `$${cost.toFixed(0)}`;
  }

  function fmtThreshold(v) {
    if (v < 0.0001) return `$${v.toFixed(6)}`;
    if (v < 0.01) return `$${v.toFixed(4)}`;
    if (v < 1) return `$${v.toFixed(3)}`;
    return `$${v.toFixed(2)}`;
  }

  function renderHeatmap() {
    const container = document.getElementById('cost-heatmap');
    if (!container) return;

    const costByDay = {};
    let maxCost = 0;
    dailyCosts.forEach((d) => {
      costByDay[d.day] = d.daily_cost;
      if (d.daily_cost > maxCost) maxCost = d.daily_cost;
    });

    // Relative thresholds based on max daily cost
    const lowThreshold = maxCost * 0.25;
    const midThreshold = maxCost * 0.5;
    const highThreshold = maxCost * 0.75;

    const LEVELS = [
      { bg: 'bg-gray-100 border border-gray-200', text: 'text-gray-300' },
      { bg: 'bg-emerald-100', text: 'text-emerald-800' },
      { bg: 'bg-emerald-300', text: 'text-emerald-900' },
      { bg: 'bg-emerald-500', text: 'text-white' },
      { bg: 'bg-emerald-700', text: 'text-white' },
    ];

    const today = new Date();
    today.setHours(23, 59, 59, 999);

    // Start 60 days back
    const start = new Date(today);
    start.setDate(start.getDate() - 59);
    start.setHours(0, 0, 0, 0);

    const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
    const DAY_LABELS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

    const weeks = [];
    const cur = new Date(start);
    while (cur <= today) {
      const week = [null, null, null, null, null, null, null];
      const weekMonth = cur.getMonth();
      const curDayOfWeek = cur.getDay();
      for (let i = curDayOfWeek; i < 7; i++) {
        week[i] = new Date(cur);
        cur.setDate(cur.getDate() + 1);
        if (cur.getMonth() !== weekMonth) break;  // Stop if month changes mid-week
      }
      weeks.push(week);
    }

    // Day label column — height matches cell h-7 + gap-1
    const dayLabelsHtml = DAY_LABELS.map((d) => `<div class="text-xs text-gray-400 w-8 h-7 flex items-center">${d}</div>`).join('');

    const monthLabelSeen = {};

    const weeksHtml = weeks.map((week) => {
      const weekMonth = week.find((day) => day !== null)?.getMonth();
      const gapClass = !monthLabelSeen[weekMonth] ? 'ml-5' : '';
      // First week always shows its month; subsequent weeks show label only when month rolls over.
      const monthLabel = !monthLabelSeen[weekMonth] ? MONTHS[weekMonth] ?? '' : '';
      monthLabelSeen[weekMonth] = true;

      const daysHtml = week.map((date) => {
        if (!date) return '<div class="w-16 h-7"></div>';

        const key = dayStr(date);
        const isWeekend = date.getDay() === 0 || date.getDay() === 6;
        const cost = costByDay[key] ?? 0;
        const isFuture = date > today;

        if (isFuture) return '<div class="w-16 h-7 rounded-sm"></div>';

        const level = getLevel(cost, maxCost, lowThreshold, midThreshold, highThreshold);
        const clr = LEVELS[level];
        const txt = fmtCell(cost);
        const tip = `<div class="flex gap-2"><b>${key}:</b><span>${cost > 0 ? `$${cost.toFixed(6)}` : 'no activity'}</span></div>`;

        return `<div class="w-16 h-7 rounded-sm ${clr.bg} ${clr.text} ${isWeekend && level === 0 ? 'bg-gray-200' : ''} flex items-center justify-center text-[.7rem] font-mono overflow-hidden cursor-default" data-tip="${esc(tip)}">${txt ? `<span class="select-none">${txt}</span>` : ''}</div>`;
      }).join('');

      return `<div class="flex flex-col gap-1 ${gapClass}">
        <div class="text-xs text-gray-500 font-medium h-7 flex items-end pb-0.5 whitespace-nowrap">${monthLabel}</div>
        ${daysHtml}
        </div>`;
    }).join('');

    container.innerHTML = `<div class="flex gap-1 items-start">
      <div class="flex flex-col gap-1 pt-8 flex-shrink-0 mr-1">${dayLabelsHtml}</div>
      ${weeksHtml}
      </div>`;

    // Populate dynamic legend with actual thresholds
    const legend = document.getElementById('heatmap-legend');
    if (legend) {
      legend.innerHTML = maxCost === 0
        ? '<span class="text-gray-400">No spending data yet.</span>'
        : `<div class="flex items-center gap-1.5"><div class="w-3 h-3 rounded-sm bg-gray-100 border border-gray-200 flex-shrink-0"></div><span>$0.00</span></div>
          <div class="flex items-center gap-1.5"><div class="w-3 h-3 rounded-sm bg-emerald-100 flex-shrink-0"></div><span>&lt;${fmtThreshold(lowThreshold)}</span></div>
          <div class="flex items-center gap-1.5"><div class="w-3 h-3 rounded-sm bg-emerald-300 flex-shrink-0"></div><span>&lt;${fmtThreshold(midThreshold)}</span></div>
          <div class="flex items-center gap-1.5"><div class="w-3 h-3 rounded-sm bg-emerald-500 flex-shrink-0"></div><span>&lt;${fmtThreshold(highThreshold)}</span></div>
          <div class="flex items-center gap-1.5"><div class="w-3 h-3 rounded-sm bg-emerald-700 flex-shrink-0"></div><span>&ge;${fmtThreshold(highThreshold)}</span></div>`;
    }
  }

  const refreshDailyCosts = makeAbortable(async (signal) => {
    const res = await fetch('/api/daily-costs?days=60', { signal });
    if (!res.ok) return;
    dailyCosts = await res.json();
    renderHeatmap();
  });

  // range-dependent, unlike refreshSessionStats/refreshDailyCosts above —
  // used both for the initial load and for switchRange below.
  const fetchTotals = makeAbortable(async (signal, rangeKey) => {
    const res = await fetch('/api/totals?range=' + encodeURIComponent(rangeKey), { signal });
    if (res.ok) renderTotals(await res.json());
  });

  // ── Live updates ──────────────────────────────────────────────────────────
  // syncedExchangeID tracks the last exchange id the by-session table has
  // incorporated, so onNewExchange can ask for exactly what changed since
  // then instead of refetching/rebuilding the whole table on every tick.
  // null means "no watermark yet" (distinct from 0, a legitimate id floor) —
  // the first tick after (re)connecting always falls in the gap between the
  // page-load fetch and the SSE connection opening, so it re-syncs with a
  // full fetch rather than silently skipping whatever changed in that gap.
  // Module-level (not reset on reconnectNav) since it's a global exchange-id
  // watermark, unrelated to the SSE range scope.
  let syncedExchangeID = null;

  // reconnectNav closes any existing SSE connection and opens a new one
  // scoped to the current range.
  let navHandle = null;
  function reconnectNav() {
    navHandle?.close();
    navHandle = initNav(range, {
      onTotals: renderTotals,
      onNewExchange: (latestId) => {
        const sinceID = syncedExchangeID;
        syncedExchangeID = latestId;
        if (sinceID === null) refreshSessionStats();
        else if (latestId > sinceID) refreshSessionStatsSince(sinceID);
        refreshDailyCosts();
      },
      onLimitersChanged: refreshLimiters,
    });
  }

  // ── Filter switching (SPA-style, no full page reload) ──────────────────────
  function applyRange(newRange) {
    range = newRange;
    updateDateLabels();
    fetchTotals(range);
    reconnectNav();
  }

  function switchRange(newRange) {
    if (!RANGES.includes(newRange) || newRange === range) return;
    applyRange(newRange);
    history.pushState(null, '', '/?range=' + encodeURIComponent(range));
  }

  if (select) {
    select.addEventListener('change', () => switchRange(select.value));
  }

  window.addEventListener('popstate', () => {
    const searchParams = new URLSearchParams(window.location.search);
    let rangeParam = searchParams.get('range');
    if (!RANGES.includes(rangeParam)) rangeParam = DEFAULT_RANGE;
    if (rangeParam === range) return;
    if (select) select.value = rangeParam;
    applyRange(rangeParam);
  });

  // ── Initial load ──────────────────────────────────────────────────────────
  async function loadDashboard() {
    await Promise.all([fetchTotals(range), refreshDailyCosts(), refreshSessionStats(), refreshLimiters()]);
  }

  loadDashboard();
  reconnectNav();

  // Recomputes just the countdown text every 30s from already-fetched data,
  // so it visibly ticks down between actual data changes without a refetch.
  setInterval(() => {
    renderGlobalLimiters();
    renderSessionRows();
  }, 30000);
})();
