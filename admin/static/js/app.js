// Shared across every admin page: formatters, the [data-tip] tooltip
// wiring, and the SSE client that drives the nav's "Proxy Service" badge
// plus each page's own live-update hook.
'use strict';

const txtEncoder = new TextEncoder();

export function pad(n) {
  return String(n).padStart(2, '0');
}

export function esc(s) {
  return String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

export function estimateBytes(str) {
  if (!str) return 0;
  return txtEncoder.encode(String(str)).length;
}

/**
 * Abbreviates a token count to 1 decimal place above 1000 (1.7k, 6.3M, 1.2B),
 * or renders it plain below that. Use for summarized counts (tables, cards);
 * use fmtInt for exact values (exchange detail page).
 * @param {?number} n - Token count.
 * @returns {string} Formatted token count, or '—' when n is null.
 */
export function fmtTokens(n) {
  if (n == null) return '—';
  const neg = n < 0;
  const abs = Math.abs(n);
  let suffix, divisor;
  if (abs >= 1000000000) { suffix = 'B'; divisor = 1000000000; }
  else if (abs >= 1000000) { suffix = 'M'; divisor = 1000000; }
  else if (abs >= 1000) { suffix = 'k'; divisor = 1000; }
  else return `${neg ? '-' : ''}${abs}`;
  const val = (abs / divisor).toFixed(1).replace(/\.0$/, '');
  return `${neg ? '-' : ''}${val}${suffix}`;
}

export function fmtInt(n) {
  if (n == null) return '—';
  return Number(n).toLocaleString('en-US');
}

export function fmtCost(c) {
  if (c == null) return '—';
  return c < 0.01 ? '<$0.01' : `$${c.toFixed(2)}`;
}

/**
 * Describes a price rule's token boundary, e.g. "≤ 1,000" or "> 1,000".
 * @param {{rule: string, rule_tokens: number}} price - Price rule record.
 * @returns {string} Human-readable rule text.
 */
export function ruleText(price) {
  return price.rule === 'under' ? `≤ ${fmtInt(price.rule_tokens)}` : `> ${fmtInt(price.rule_tokens)}`;
}

export function fmtTime(ts) {
  if (!ts) return '—';
  const d = new Date(ts * 1000);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/**
 * Human-readable time remaining until a Unix-seconds timestamp, e.g.
 * "in 3 mins", "in 2 hrs", "in 5 days".
 * @param {?number} ts - Target Unix timestamp in seconds.
 * @returns {string} Countdown text, or '—' when ts is null.
 */
export function fmtCountdown(ts) {
  if (!ts) return '—';
  const diff = ts - Date.now() / 1000;
  if (diff <= 0) return 'refreshing…';
  const mins = Math.round(diff / 60);
  if (mins < 1) return 'in <1 min';
  if (mins < 60) return `in ${mins} min${mins === 1 ? '' : 's'}`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `in ${hrs} hr${hrs === 1 ? '' : 's'}`;
  const days = Math.round(hrs / 24);
  return `in ${days} day${days === 1 ? '' : 's'}`;
}

/**
 * Formats a limiter's active period as HTML.
 * @param {{active_start_hour: number|null, active_end_hour: number|null, is_active: boolean, within_active_period: boolean}} l - Limiter record.
 * @returns {string} HTML markup for the active period badge.
 */
export function fmtActivePeriod(l) {
  if (l.active_start_hour == null) {
    return l.is_active
      ? '<span class="px-1.5 py-0.5 bg-blue-50 text-blue-700 rounded font-medium">Always</span>'
      : '<span class="px-1.5 py-0.5 bg-gray-50 text-gray-400 rounded font-medium">Always</span>';
  }
  const color = l.within_active_period && l.is_active ? 'bg-emerald-50 text-emerald-700' : 'bg-gray-50 text-gray-400';
  return `<span class="px-1.5 py-0.5 ${color} rounded font-medium">${pad(l.active_start_hour)}:00 – ${pad(l.active_end_hour)}:59</span>`;
}

/**
 * Progress bar + "spent of limit" caption for a limiter's current spend.
 * @param {{current_cost: number, limit_amount: number}} l - Limiter record.
 * @returns {string} Progress-bar markup.
 */
export function progressBar(l, height = 'h-1.5') {
  const pct = l.limit_amount > 0 ? Math.min(100, (l.current_cost / l.limit_amount) * 100) : 0;
  const barColor = pct >= 100 ? 'bg-red-500' : pct >= 80 ? 'bg-amber-500' : 'bg-emerald-500';
  return `<div class="w-full flex flex-col items-end">
    <div class="${height} w-full bg-gray-200 rounded-full overflow-hidden">
      <div class="h-full ${l.within_active_period && l.is_active ? barColor : 'bg-gray-400'}" style="width:${pct}%"></div>
    </div>
    <div class="text-xs text-gray-500 mt-1">${fmtCost(l.current_cost)} of ${fmtCost(l.limit_amount)}</div>
  </div>`;
}

export function fmtBytes(bytes) {
  if (bytes == null) return '—';
  if (bytes < 1024) return `${bytes} Bytes`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(2)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(2)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

export function fmtSessionId(sessionId, maxLength = 15) {
  if (!sessionId) return '-';
  return sessionId.length > maxLength ? `${sessionId.slice(0, Math.floor(maxLength / 2))} … ${sessionId.slice(-Math.floor(maxLength / 2))}` : sessionId;
}

/**
 * Sums two nullable costs. Result is nil only when both inputs are nil, so an
 * unpriced cache rate on one side still shows the other's cost instead of
 * collapsing to "—".
 * @param {?number} a - First cost component.
 * @param {?number} b - Second cost component.
 * @returns {?number} Sum of the non-null components, or null when both are null.
 */
export function addCost(a, b) {
  if (a == null && b == null) return null;
  return (a ?? 0) + (b ?? 0);
}

export function costTooltip(row) {
  const parts = [];
  if (row.input_cost != null) parts.push(`<div class="flex justify-between"><b>Input:</b><span>${fmtCost(row.input_cost)}</span></div>`);
  if (row.cache_creation_cost != null) parts.push(`<div class="flex justify-between"><b>Cache create:</b><span>${fmtCost(row.cache_creation_cost)}</span></div>`);
  if (row.cache_read_cost != null) parts.push(`<div class="flex justify-between"><b>Cache read:</b><span>${fmtCost(row.cache_read_cost)}</span></div>`);
  if (row.output_cost != null) parts.push(`<div class="flex justify-between mt-1.5"><b>Output:</b><span>${fmtCost(row.output_cost)}</span></div>`);
  return parts.length ? '<div class="w-32 flex flex-col gap-0.5">' + parts.join('') + '</div>' : '';
}

export function tokensTooltip(row) {
  const parts = [];
  if (row.input_tokens != null) parts.push(`<div class="flex justify-between"><b>Input:</b><span>${fmtTokens(row.input_tokens)}</span></div>`);
  if (row.cache_creation_tokens != null) parts.push(`<div class="flex justify-between"><b>Cache create:</b><span>${fmtTokens(row.cache_creation_tokens)}</span></div>`);
  if (row.cache_read_tokens != null) parts.push(`<div class="flex justify-between"><b>Cache read:</b><span>${fmtTokens(row.cache_read_tokens)}</span></div>`);
  if (row.output_tokens != null) parts.push(`<div class="flex justify-between mt-1.5"><b>Output:</b><span>${fmtTokens(row.output_tokens)}</span></div>`);
  return parts.length ? '<div class="w-32 flex flex-col gap-0.5">' + parts.join('') + '</div>' : '';
}

// Single floating tooltip driven by [data-tip] elements: one fixed-position
// element repositioned on hover/focus instead of one per triggering cell.
(() => {
  const tip = document.getElementById('app-tooltip');
  if (!tip) return;
  let activeEl = null;

  function positionTip(el) {
    const rect = el.getBoundingClientRect();
    const tipRect = tip.getBoundingClientRect();
    let left = rect.left + rect.width / 2 - tipRect.width / 2;
    left = Math.max(4, Math.min(left, window.innerWidth - tipRect.width - 4));
    let top = rect.top - tipRect.height - 8;
    if (top < 4) top = rect.bottom + 8;
    tip.style.left = `${left}px`;
    tip.style.top = `${top}px`;
  }

  function showTip(el) {
    const textOrHtml = el.getAttribute('data-tip');
    if (!textOrHtml) return;
    tip.innerHTML = textOrHtml;
    activeEl = el;
    tip.classList.remove('opacity-0');
    tip.classList.add('opacity-100');
    positionTip(el);
  }

  function hideTip() {
    activeEl = null;
    tip.classList.remove('opacity-100');
    tip.classList.add('opacity-0');
  }

  document.addEventListener('mouseover', (e) => {
    const el = e.target.closest('[data-tip]');
    if (el) showTip(el);
  });
  document.addEventListener('mouseout', (e) => {
    if (activeEl && e.target.closest('[data-tip]') === activeEl) hideTip();
  });
  document.addEventListener('focusin', (e) => {
    const el = e.target.closest('[data-tip]');
    if (el) showTip(el);
  });
  document.addEventListener('focusout', (e) => {
    if (activeEl && e.target.closest('[data-tip]') === activeEl) hideTip();
  });
  document.addEventListener('scroll', () => {
    if (activeEl) positionTip(activeEl);
  }, true);
  window.addEventListener('resize', hideTip);
})();

export function connectSSE(range, onMessage) {
  const eventSource = new EventSource('/api/stream' + (range ? `?range=${encodeURIComponent(range)}` : ''));
  eventSource.onmessage = (e) => onMessage(JSON.parse(e.data));
  eventSource.onerror = () => onMessage({ proxy_status: 'unreachable' });
  return eventSource;
}

export function updateProxyStatusBadge(statusValue) {
  const el = document.getElementById('proxy-status');
  if (!el) return;
  switch (statusValue) {
    case 'ok':
      el.textContent = 'Proxy Service: OK';
      el.className = 'ml-auto px-2 py-1 border border-emerald-300 border-dashed text-xs text-gray-500 rounded-lg bg-emerald-100 text-emerald-800';
      break;
    case 'degraded':
      el.textContent = 'Proxy Service: Degraded';
      el.className = 'ml-auto px-2 py-1 border border-yellow-300 border-dashed text-xs text-gray-500 rounded-lg bg-yellow-100 text-yellow-800';
      break;
    case 'unreachable':
      el.textContent = 'Proxy Service: Unreachable';
      el.className = 'ml-auto px-2 py-1 border border-red-300 border-dashed text-xs text-gray-500 rounded-lg bg-red-100 text-red-800 animate-pulse';
      break;
    default:
      el.textContent = 'Proxy Service: Unknown';
      el.className = 'ml-auto px-2 py-1 border border-gray-300 border-dashed text-xs text-gray-500 rounded-lg bg-gray-100 text-gray-800';
  }
}

/**
 * Wires the nav's proxy-status badge on every page and, when given handlers,
 * forwards SSE-pushed totals/new-exchange/limiters-changed events to the
 * calling page. onNewExchange/onLimitersChanged only fire when their
 * respective version fields actually increase, so pages don't each need
 * their own dedup bookkeeping.
 * @param {?string} range - Dashboard range key to scope totals to, or null.
 * @param {{onTotals?: Function, onNewExchange?: Function, onLimitersChanged?: Function}} [handlers] - Optional event callbacks.
 * @returns {EventSource} The underlying SSE connection, for closing on range change.
 */
export function initNav(range, handlers = {}) {
  const { onTotals, onNewExchange, onLimitersChanged } = handlers;
  let knownLatestId = 0;
  let knownLimitersVersion = 0;
  return connectSSE(range, (data) => {
    updateProxyStatusBadge(data.proxy_status);
    if (data.totals && typeof onTotals === 'function') onTotals(data.totals);
    if (data.latest_exchange_id && data.latest_exchange_id > knownLatestId) {
      knownLatestId = data.latest_exchange_id;
      if (typeof onNewExchange === 'function') onNewExchange(data.latest_exchange_id);
    }
    if (data.limiters_version && data.limiters_version > knownLimitersVersion) {
      knownLimitersVersion = data.limiters_version;
      if (typeof onLimitersChanged === 'function') onLimitersChanged(data.limiters_version);
    }
  });
}

/**
 * Lighter alternative to initNav for pages that only need the proxy-status
 * badge and have no use for live totals/new-exchange events (exchange
 * detail, prices): polls /api/health instead of holding open an SSE
 * connection.
 * @param {number} [intervalMs=15000] - Poll interval in milliseconds.
 * @returns {number} The interval id, for cleanup via clearInterval.
 */
export function initNavPolling(intervalMs = 15000) {
  async function poll() {
    try {
      const res = await fetch('/api/health');
      updateProxyStatusBadge(res.ok ? (await res.json()).proxy : 'unreachable');
    } catch {
      updateProxyStatusBadge('unreachable');
    }
  }
  poll();
  return setInterval(poll, intervalMs);
}

/**
 * Clamps a requested page into range: a stale bookmarked page (or one typed
 * past the last page) degrades to the nearest valid page instead of
 * rendering empty.
 * @param {number} reqPage - Requested page number.
 * @param {number} size - Rows per page.
 * @param {number} total - Total matching rows.
 * @returns {{page: number, totalPages: number, from: number, to: number}} Clamped pagination state.
 */
export function computePagination(reqPage, size, total) {
  const totalPages = Math.max(1, Math.ceil(total / size));
  const page = Math.min(Math.max(reqPage, 1), totalPages);
  if (total === 0) return { page, totalPages, from: 0, to: 0 };
  const from = (page - 1) * size + 1;
  const to = Math.min(from + size - 1, total);
  return { page, totalPages, from, to };
}

/**
 * Renders a "rows per page" select, an "X–Y of Z results" caption, and
 * First/Previous/page-jump/Next/Last controls into containerId. Rebuilds the
 * container's innerHTML on every call and wires only the two elements that
 * get recreated each time (size select, page-jump input) — First/Previous/
 * Next/Last links are handled separately by wirePaginationNav, since that
 * listener only needs to be attached once.
 * @param {string} containerId - Id of the element to render into.
 * @param {{page: number, totalPages: number, from: number, to: number, total: number, pageSize: number}} state - Pagination state, e.g. from computePagination plus the active page size.
 * @param {number[]} pageSizes - Selectable rows-per-page options.
 * @param {function(number, number): void} onSizeOrJumpChange - Called with (page, size) when the size select or page-jump input changes.
 */
export function renderPaginationControls(containerId, state, pageSizes, onSizeOrJumpChange) {
  const container = document.getElementById(containerId);
  if (!container) return;
  const { page, totalPages, from, to, total, pageSize } = state;

  const navLink = (label, targetPage, enabled) => enabled
    ? `<a href="#" data-page="${targetPage}" class="pagination-link text-emerald-600 hover:underline">${label}</a>`
    : `<span class="text-gray-300">${label}</span>`;

  container.innerHTML = `
    <div class="flex items-center gap-2 text-gray-500">
      <label for="${containerId}-page-size-select">Rows per page</label>
      <select id="${containerId}-page-size-select" class="border border-gray-300 rounded px-2 py-1 text-sm focus:outline-none focus:ring-1 focus:ring-emerald-500">
        ${pageSizes.map((s) => `<option value="${s}" ${s === pageSize ? 'selected' : ''}>${s}</option>`).join('')}
      </select>
      <span>${total > 0 ? `${from}–${to} of ${total}` : '0 of 0'} results</span>
    </div>
    <div class="flex items-center gap-3">
      ${navLink('First', 1, page > 1)}
      ${navLink('Previous', page - 1, page > 1)}
      <span class="flex items-center gap-1.5 text-gray-500">
        Page
        <input id="${containerId}-page-jump-input" type="number" min="1" max="${totalPages}" value="${page}"
          class="border border-gray-300 rounded px-2 py-1 text-sm w-16 text-center focus:outline-none focus:ring-1 focus:ring-emerald-500">
        of ${totalPages}
      </span>
      ${navLink('Next', page + 1, page < totalPages)}
      ${navLink('Last', totalPages, page < totalPages)}
    </div>`;

  const sizeSelect = document.getElementById(`${containerId}-page-size-select`);
  if (sizeSelect) {
    sizeSelect.addEventListener('change', () => onSizeOrJumpChange(1, Number(sizeSelect.value)));
  }

  const jump = document.getElementById(`${containerId}-page-jump-input`);
  if (jump) {
    jump.addEventListener('focus', () => jump.select());
    jump.addEventListener('keydown', (e) => {
      if (e.key !== 'Enter') return;
      e.preventDefault();
      jump.blur();
    });
    jump.addEventListener('change', () => {
      let p = parseInt(jump.value, 10);
      if (!Number.isFinite(p)) p = 1;
      p = Math.min(Math.max(p, 1), totalPages);
      onSizeOrJumpChange(p, pageSize);
    });
  }
}

/**
 * Wires a one-time delegated click listener for First/Previous/Next/Last
 * .pagination-link anchors inside #containerId. Kept separate from
 * renderPaginationControls so repeated re-renders (which replace the
 * container's innerHTML but not the container itself) don't stack duplicate
 * listeners.
 * @param {string} containerId - Id of the pagination container.
 * @param {function(number): void} onNavigate - Called with the target page number.
 */
export function wirePaginationNav(containerId, onNavigate) {
  document.addEventListener('click', (e) => {
    const link = e.target.closest(`#${containerId} .pagination-link`);
    if (!link) return;
    e.preventDefault();
    onNavigate(Number(link.dataset.page));
  });
}

/**
 * Wraps an async task so a new call cancels its predecessor via AbortSignal,
 * instead of letting a slow, superseded response overwrite fresher data.
 * The wrapped function forwards its own args after the injected signal, e.g.
 * makeAbortable(async (signal, rangeKey) => {...}) is called as fn(rangeKey).
 * @param {Function} taskFn - Async function taking (signal, ...args).
 * @returns {Function} Abortable wrapper with the same trailing-args signature.
 */
export function makeAbortable(taskFn) {
  let currentController = null;
  return async (...args) => {
    currentController?.abort();
    const controller = new AbortController();
    currentController = controller;
    try {
      await taskFn(controller.signal, ...args);
    } catch (err) {
      if (err.name !== 'AbortError') throw err;
    }
  };
}

/**
 * Extracts the "error" field from a JSON error response, falling back to a
 * caller-supplied message when the body is missing or isn't valid JSON.
 * @param {Response} res - Fetch response to read.
 * @param {string} fallback - Message to use when no error field is present.
 * @returns {Promise<string>} The error message to display.
 */
export async function extractErrorMessage(res, fallback) {
  const body = await res.json().catch(() => ({}));
  return body.error || fallback;
}

/**
 * Wraps fn so a rapid burst of calls only runs it once, delayMs after the
 * last call — e.g. re-rendering on every keystroke of a search input.
 * @param {Function} fn - Function to debounce.
 * @param {number} [delayMs=200] - Quiet period required before fn runs.
 * @returns {Function} Debounced wrapper with the same signature as fn.
 */
export function debounce(fn, delayMs = 200) {
  let timer = null;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), delayMs);
  };
}

/**
 * POSTs payload as JSON. Returns the raw Response for the caller to check
 * ok/status and read the body.
 * @param {string} url - Request URL.
 * @param {object} payload - Body to JSON-encode.
 * @returns {Promise<Response>} The fetch response.
 */
export function postJSON(url, payload) {
  return fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
}

/**
 * PUTs payload as JSON. Returns the raw Response for the caller to check
 * ok/status and read the body.
 * @param {string} url - Request URL.
 * @param {object} payload - Body to JSON-encode.
 * @returns {Promise<Response>} The fetch response.
 */
export function putJSON(url, payload) {
  return fetch(url, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
}

/**
 * Builds a setDialogMessage(type, text) function for one dialog: shows text
 * styled per type, or hides the element when text is falsy.
 * @param {string} elementId - Id of the message element inside the dialog.
 * @param {Object<string, string[]>} styles - classList to apply per type.
 * @returns {function(?string, string): void} setDialogMessage(type, text).
 */
export function makeDialogMessage(elementId, styles) {
  return function setDialogMessage(type, text) {
    const el = document.getElementById(elementId);
    if (!el) return;
    Object.values(styles).forEach((cls) => el.classList.remove(...cls));
    if (!text) {
      el.classList.add('hidden');
      el.textContent = '';
      return;
    }
    el.classList.remove('hidden');
    el.classList.add(...styles[type]);
    el.textContent = text;
  };
}
