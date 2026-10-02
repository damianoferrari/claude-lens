import { pad, esc, fmtTokens, fmtCost, fmtCountdown, fmtTime, addCost, makeAbortable, initNav, progressBar, fmtSessionId, fmtActivePeriod, computePagination, renderPaginationControls, wirePaginationNav, tokensTooltip, costTooltip, extractErrorMessage, makeDialogMessage, postJSON } from './app.js';

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
    return `<div class="p-4 bg-surface rounded-lg border border-line">
      ${progressBar(l, 'h-2')}
      <div class="flex justify-between items-center mt-4">
        <span class="text-xs text-fg-subtle">${esc(l.within_active_period && l.is_active ? fmtCountdown(l.next_refresh_at) : 'currently inactive')}</span>
        <span class="text-xs">${fmtActivePeriod(l)}</span>
      </div>
      </div>`;
  }

  function renderGlobalLimiters() {
    const container = document.getElementById('global-limiters');
    if (!container) return;
    container.innerHTML = globalLimiters.length
      ? globalLimiters.map(limiterCard).join('')
      : '<p class="text-fg-subtle text-sm">No global limiters configured.</p>';
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

  // editingSessionId/editingDraft track an in-progress Ctrl+click rename so
  // renderSessionRows() (driven by SSE deltas and the 30s countdown tick)
  // can re-render that row back into edit mode instead of clobbering it.
  let editingSessionId = null;
  let editingDraft = '';

  // Session IDs checked for bulk delete among the rows on the current page.
  let selectedSessionIds = new Set();
  const ACTIVE_SESSION_WINDOW_SECONDS = 30 * 60;

  function sessionLimiterCell(session) {
    const l = limitersBySession.get(session.session_id);
    if (!l) return '<span class="text-fg-subtle">—</span>';
    return `${progressBar(l)}<p class="text-xs text-fg-subtle mt-1">${esc(fmtCountdown(l.within_active_period && l.is_active ? l.next_refresh_at : null))}</p>`;
  }

  function buildSessionRow(session) {
    const cacheTok = (session.total_cache_creation_tokens ?? 0) + (session.total_cache_read_tokens ?? 0);
    const inputTok = session.total_input_tokens ?? 0;
    const outputTok = session.total_output_tokens ?? 0;
    const totalTok = inputTok + outputTok + cacheTok;
    const cost = session.total_cost ?? 0;
    const costStr = cost > 0 ? fmtCost(cost) : '—';
    const exchangeCount = session.exchange_count || 0;
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
    // context_* fields are the last exchange's own token counts, not a sum
    // across the session — see SessionStat's Context* fields in exchanges.go.
    const ctxCacheTok = (session.context_cache_creation_tokens ?? 0) + (session.context_cache_read_tokens ?? 0);
    const ctxInputTok = session.context_input_tokens ?? 0;
    const ctxOutputTok = session.context_output_tokens ?? 0;
    const contextSize = ctxInputTok + ctxOutputTok + ctxCacheTok;
    const contextRow = {
      input_tokens: session.context_input_tokens,
      output_tokens: session.context_output_tokens,
      cache_creation_tokens: session.context_cache_creation_tokens,
      cache_read_tokens: session.context_cache_read_tokens,
    };
    const sessionQuery = 'session = ' + JSON.stringify(session.session_id);
    const isEditing = session.session_id === editingSessionId;
    const nameHtml = isEditing
      ? `<input type="text" class="w-full max-w-56 text-sm px-1.5 py-0.5 border border-emerald-400 dark:border-emerald-600 rounded focus:outline-none focus:ring-1 focus:ring-emerald-400 session-name-input" value="${esc(editingDraft)}" maxlength="200">`
      : `<a href="/exchanges?q=${encodeURIComponent(sessionQuery)}" class="text-emerald-600 dark:text-emerald-400 font-medium hover:underline">${esc(session.session_name || fmtSessionId(session.session_id, 24))}</a>${session.session_name ? `<span class="block text-xs text-fg-subtle font-mono">${esc(fmtSessionId(session.session_id, 24))}</span>` : ''}`;
    return `<tr class="hover:bg-surface-hover">
      <td class="h-px">
        <label class="flex h-full cursor-pointer items-center px-4 py-2">
          <input type="checkbox" class="session-select-checkbox cursor-pointer" aria-label="Select session" data-session-id="${esc(session.session_id)}" ${selectedSessionIds.has(session.session_id) ? 'checked' : ''}>
        </label>
      </td>
      <td class="px-4 py-2 session-name-cell" data-session-id="${esc(session.session_id)}">${nameHtml}</td>
      <td class="text-right text-fg px-4 py-2">${session.exchange_count}</td>
      <td class="text-fg px-4 py-2">${esc(session.model || '—')}</td>
      <td class="text-right text-fg px-4 py-2" data-tip="${esc(tokensTooltip(tokensRow))}">
        ${fmtTokens(totalTok)}
      </td>
      <td class="text-right text-fg px-4 py-2" data-tip="${esc(tokensTooltip(contextRow))}">
        ${fmtTokens(contextSize)}
      </td>
      <td class="text-right text-fg px-4 py-2" data-tip="${esc(costTooltip(costsRow))}">
        ${costStr}
        <p class="text-xs text-fg-subtle">avg ${avgCostStr}</p>
      </td>
      <td class="text-fg-subtle whitespace-nowrap px-4 py-2">${fmtTime(session.last_updated)}</td>
      <td class="w-36 whitespace-nowrap text-right px-4 py-2">${sessionLimiterCell(session)}</td>
      </tr>`;
  }

  function renderSessionRows() {
    const tbody = document.getElementById('session-stats-tbody');
    if (!tbody) return;
    // Remove selections for sessions no longer present on this page.
    const visibleIds = new Set(lastSessionRows.map((r) => r.session_id));
    for (const id of selectedSessionIds) {
      if (!visibleIds.has(id)) selectedSessionIds.delete(id);
    }
    tbody.innerHTML = lastSessionRows.length
      ? lastSessionRows.map(buildSessionRow).join('')
      : '<tr><td colspan="9" class="text-center text-fg-subtle px-4 py-8">No sessions yet.</td></tr>';
    if (editingSessionId !== null) {
      const input = tbody.querySelector('.session-name-input');
      if (input) {
        input.focus();
        input.setSelectionRange(input.value.length, input.value.length);
      }
    }
    renderSelectionUI();
  }

  /**
   * Enters inline-rename mode for a session's name cell.
   * @param {string} sessionId - Session to rename.
   */
  function startEditingSession(sessionId) {
    const session = lastSessionRows.find((r) => r.session_id === sessionId);
    editingSessionId = sessionId;
    editingDraft = session?.session_name || '';
    renderSessionRows();
  }

  /**
   * Exits inline-rename mode without saving.
   */
  function stopEditingSession() {
    editingSessionId = null;
    editingDraft = '';
    renderSessionRows();
  }

  /**
   * Commits the rename input's current value via PATCH, then patches the
   * matching row in lastSessionRows with the server-confirmed name.
   * @param {HTMLInputElement} input - The rename input being committed.
   */
  async function commitSessionName(input) {
    const sessionId = editingSessionId;
    const name = input.value.trim();
    const session = lastSessionRows.find((r) => r.session_id === sessionId);
    if (name === (session?.session_name || '')) {
      stopEditingSession();
      return;
    }
    editingSessionId = null;
    editingDraft = '';
    try {
      const res = await fetch(`/api/sessions/${encodeURIComponent(sessionId)}/name`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name }),
      });
      if (res.ok) {
        const data = await res.json();
        lastSessionRows = lastSessionRows.map((r) => (r.session_id === sessionId ? { ...r, session_name: data.session_name } : r));
      }
    } finally {
      renderSessionRows();
    }
  }

  const sessionStatsTbody = document.getElementById('session-stats-tbody');
  if (sessionStatsTbody) {
    sessionStatsTbody.addEventListener('click', (e) => {
      if (!(e.ctrlKey || e.metaKey)) return;
      const cell = e.target.closest('.session-name-cell');
      if (!cell) return;
      e.preventDefault();
      startEditingSession(cell.dataset.sessionId);
    });

    sessionStatsTbody.addEventListener('keydown', (e) => {
      if (!e.target.classList.contains('session-name-input')) return;
      if (e.key === 'Enter') {
        e.preventDefault();
        commitSessionName(e.target);
      } else if (e.key === 'Escape') {
        e.preventDefault();
        stopEditingSession();
      }
    });

    sessionStatsTbody.addEventListener('focusout', (e) => {
      if (!e.target.classList.contains('session-name-input')) return;
      if (editingSessionId === null) return;
      stopEditingSession();
    });

    sessionStatsTbody.addEventListener('change', (e) => {
      if (!e.target.classList.contains('session-select-checkbox')) return;
      const sessionId = e.target.dataset.sessionId;
      if (e.target.checked) selectedSessionIds.add(sessionId);
      else selectedSessionIds.delete(sessionId);
      renderSelectionUI();
    });
  }

  // ── Bulk delete ─────────────────────────────────────────────────────────
  const sessionSelectAll = document.getElementById('session-select-all');
  const sessionBulkActions = document.getElementById('session-bulk-actions');
  const sessionSelectedCount = document.getElementById('session-selected-count');
  const bulkDeleteBtn = document.getElementById('bulk-delete-btn');

  /**
   * Syncs the header select-all checkbox (checked/indeterminate) and the
   * bulk-actions bar (visibility + count) with selectedSessionIds.
   */
  function renderSelectionUI() {
    const count = selectedSessionIds.size;
    if (sessionBulkActions) sessionBulkActions.classList.toggle('hidden', count === 0);
    if (sessionBulkActions) sessionBulkActions.classList.toggle('flex', count > 0);
    if (sessionSelectedCount) sessionSelectedCount.textContent = `${count} selected`;
    if (sessionSelectAll) {
      sessionSelectAll.checked = lastSessionRows.length > 0 && count === lastSessionRows.length;
      sessionSelectAll.indeterminate = count > 0 && count < lastSessionRows.length;
    }
  }

  function clearSelection() {
    selectedSessionIds.clear();
    renderSelectionUI();
  }

  if (sessionSelectAll) {
    sessionSelectAll.addEventListener('change', () => {
      if (sessionSelectAll.checked) lastSessionRows.forEach((r) => selectedSessionIds.add(r.session_id));
      else selectedSessionIds.clear();
      renderSessionRows();
    });
  }

  const setBulkDeleteMessage = makeDialogMessage('bulk-delete-dialog-message', {
    warning: ['text-amber-700 dark:text-amber-400', 'bg-amber-50 dark:bg-amber-500/15', 'border-amber-200 dark:border-amber-800'],
    error: ['text-red-700 dark:text-red-400', 'bg-red-50 dark:bg-red-500/15', 'border-red-200 dark:border-red-800'],
    success: ['text-emerald-700 dark:text-emerald-400', 'bg-emerald-50 dark:bg-emerald-500/15', 'border-emerald-200 dark:border-emerald-800'],
  });

  if (bulkDeleteBtn) {
    bulkDeleteBtn.onclick = () => {
      const dialog = document.getElementById('bulk-delete-dialog');
      const checkbox = document.getElementById('bulk-delete-also-delete-claude-session');
      const countEl = document.getElementById('bulk-delete-session-count');
      if (checkbox) checkbox.checked = false;
      setBulkDeleteMessage(null, '');
      if (countEl) countEl.textContent = String(selectedSessionIds.size);

      function updateActiveWarning() {
        if (!checkbox?.checked) {
          setBulkDeleteMessage(null, '');
          return;
        }
        const anyActive = [...selectedSessionIds].some((id) => {
          const row = lastSessionRows.find((r) => r.session_id === id);
          return row && (Date.now() / 1000 - row.last_updated) < ACTIVE_SESSION_WINDOW_SECONDS;
        });
        setBulkDeleteMessage(anyActive ? 'warning' : null, anyActive
          ? 'One or more of the selected sessions had Claude Code activity in the last 30 minutes and may still be open in a terminal. Deleting their files now could corrupt or lose data from those sessions.'
          : '');
      }
      if (checkbox) checkbox.onchange = updateActiveWarning;

      dialog?.showModal();
    };
  }

  const submitBulkDeleteBtn = document.getElementById('submit-bulk-delete-btn');
  if (submitBulkDeleteBtn) {
    submitBulkDeleteBtn.onclick = async () => {
      const checkbox = document.getElementById('bulk-delete-also-delete-claude-session');
      const res = await postJSON('/api/exchanges/bulk-delete', {
        session_ids: [...selectedSessionIds],
        also_delete_claude_session: !!checkbox?.checked,
      });
      if (!res.ok) {
        setBulkDeleteMessage('error', await extractErrorMessage(res, res.statusText || 'Failed to delete selected sessions.'));
        return;
      }
      setBulkDeleteMessage('success', 'Selected sessions deleted.');
      setTimeout(() => {
        document.getElementById('bulk-delete-dialog')?.close();
        clearSelection();
        refreshSessionStats();
        fetchTotals(range);
        refreshDailyCosts();
      }, 800);
    };
  }

  function renderSessionPagination() {
    const pagination = computePagination(sessionPage, sessionPageSize, sessionTotal);
    sessionPage = pagination.page;
    renderPaginationControls('session-pagination-controls', { ...pagination, total: sessionTotal, pageSize: sessionPageSize }, SESSION_PAGE_SIZES, (page, size) => {
      sessionPage = page;
      sessionPageSize = size;
      clearSelection();
      refreshSessionStats();
    });
  }

  wirePaginationNav('session-pagination-controls', (page) => {
    sessionPage = page;
    clearSelection();
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
   * Linear-interpolation percentile over an ascending-sorted array.
   * @param {number[]} sorted - Values sorted ascending.
   * @param {number} p - Percentile in [0, 1].
   * @returns {number} Interpolated value, or 0 for an empty array.
   */
  function percentile(sorted, p) {
    if (!sorted.length) return 0;
    const idx = (sorted.length - 1) * p;
    const lo = Math.floor(idx);
    const hi = Math.ceil(idx);
    if (lo === hi) return sorted[lo];
    return sorted[lo] + (sorted[hi] - sorted[lo]) * (idx - lo);
  }

  /**
   * Buckets a daily cost into one of 5 heatmap levels using percentile
   * thresholds computed from the visible range's nonzero daily costs.
   * @param {number} cost - This day's cost.
   * @param {number} lowThreshold - 25th-percentile upper bound of level 1.
   * @param {number} midThreshold - 50th-percentile upper bound of level 2.
   * @param {number} highThreshold - 75th-percentile upper bound of level 3.
   * @returns {number} Heatmap level from 0 (no activity) to 4 (highest).
   */
  function getLevel(cost, lowThreshold, midThreshold, highThreshold) {
    if (!cost || cost === 0) return 0;
    if (cost < lowThreshold) return 1;
    if (cost < midThreshold) return 2;
    if (cost < highThreshold) return 3;
    return 4;
  }

  /**
   * Adaptive precision up to 2 decimals, then 0 decimals for larger values.
   * @param {number} cost - Cost to format.
   * @returns {string} Formatted cost (e.g. "<$0.01" for sub-cent, "" for zero).
   */
  function fmtCell(cost) {
    if (!cost || cost === 0) return '';
    if (cost < 0.01) return `<$0.01`;
    if (cost < 10) return `$${cost.toFixed(2)}`;
    if (cost < 100) return `$${cost.toFixed(1)}`;
    if (cost < 1000) return `$${cost.toFixed(0)}`;
    return `(╯°□°)╯`;
  }

  function fmtThreshold(v) {
    if (v < 0.01) return `$<0.01`;
    if (v < 10) return `$${v.toFixed(2)}`;
    if (v < 100) return `$${v.toFixed(1)}`;
    return `$${v.toFixed(0)}`;
  }

  function renderHeatmap() {
    const container = document.getElementById('cost-heatmap');
    if (!container) return;

    const costByDay = {};
    let maxCost = 0;
    const nonZeroCosts = [];
    dailyCosts.forEach((d) => {
      costByDay[d.day] = d.daily_cost;
      if (d.daily_cost > maxCost) maxCost = d.daily_cost;
      if (d.daily_cost > 0) nonZeroCosts.push(d.daily_cost);
    });
    const sortedCosts = nonZeroCosts.toSorted((a, b) => a - b);

    // Quantile thresholds based on the spread of nonzero daily costs, so a
    // single outlier day doesn't compress every other day into level 1.
    const lowThreshold = percentile(sortedCosts, 0.25);
    const midThreshold = percentile(sortedCosts, 0.5);
    const highThreshold = percentile(sortedCosts, 0.75);

    const LEVELS = [
      { bg: 'bg-surface-hover border border-line', text: 'text-fg-subtle' },
      { bg: 'bg-emerald-100 dark:bg-emerald-200', text: 'text-emerald-800 dark:text-emerald-900' },
      { bg: 'bg-emerald-300 dark:bg-emerald-400', text: 'text-emerald-900 dark:text-emerald-950' },
      { bg: 'bg-emerald-500 dark:bg-emerald-600', text: 'text-white' },
      { bg: 'bg-emerald-700 dark:bg-emerald-800', text: 'text-white' },
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
    const dayLabelsHtml = DAY_LABELS.map((d) => `<div class="flex items-center w-8 h-7 text-xs text-fg-subtle">${d}</div>`).join('');

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

        const level = getLevel(cost, lowThreshold, midThreshold, highThreshold);
        const clr = LEVELS[level];
        const txt = fmtCell(cost);
        const tip = `<div class="flex gap-2"><b>${key}:</b><span>${cost > 0 ? `$${cost.toFixed(6)}` : 'no activity'}</span></div>`;

        return `<div class="flex items-center justify-center overflow-hidden w-16 h-7 text-xs font-mono cursor-default rounded-sm ${clr.bg} ${clr.text} ${isWeekend && level === 0 ? 'bg-surface-active' : ''}" data-tip="${esc(tip)}">${txt ? `<span class="select-none">${txt}</span>` : ''}</div>`;
      }).join('');

      return `<div class="flex flex-col gap-1 ${gapClass}">
        <div class="flex items-end h-7 text-xs text-fg-muted font-medium whitespace-nowrap pb-0.5">${monthLabel}</div>
        ${daysHtml}
        </div>`;
    }).join('');

    container.innerHTML = `<div class="flex items-start gap-1">
      <div class="flex flex-col gap-1 pt-8 flex-shrink-0 mr-1">${dayLabelsHtml}</div>
      ${weeksHtml}
      </div>`;

    // Populate dynamic legend with actual thresholds
    const legend = document.getElementById('heatmap-legend');
    if (legend) {
      if (maxCost === 0) {
        legend.innerHTML = '<span class="text-fg-subtle">No spending data yet.</span>';
      } else {
        const entries = [
          { bg: LEVELS[0].bg, label: '$0.00' },
          { bg: LEVELS[1].bg, label: `&lt;${fmtThreshold(lowThreshold)}` },
          { bg: LEVELS[2].bg, label: `&lt;${fmtThreshold(midThreshold)}` },
          { bg: LEVELS[3].bg, label: `&lt;${fmtThreshold(highThreshold)}` },
          { bg: LEVELS[4].bg, label: `&ge;${fmtThreshold(highThreshold)}` },
        ];

        // Collapse consecutive levels whose formatted threshold is identical
        // (e.g. several sub-cent thresholds all display as "<$0.01") into a
        // single legend row with stacked swatches instead of repeating the
        // same label.
        const groups = [];
        entries.forEach((entry) => {
          const lastGroup = groups[groups.length - 1];
          if (lastGroup && lastGroup.label === entry.label) {
            lastGroup.swatches.push(entry.bg);
          } else {
            groups.push({ label: entry.label, swatches: [entry.bg] });
          }
        });

        legend.innerHTML = groups.map((g) => `<div class="flex items-center gap-1.5">
          <div class="flex gap-0.5">${g.swatches.map((bg) => `<div class="w-3 h-3 rounded-sm ${bg} flex-shrink-0"></div>`).join('')}</div>
          <span>${g.label}</span>
          </div>`).join('');
      }
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
