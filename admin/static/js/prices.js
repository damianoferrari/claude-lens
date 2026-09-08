import { esc, extractErrorMessage, fmtInt, fmtTime, initNavPolling, makeDialogMessage, postJSON, putJSON, ruleText } from './app.js';

'use strict';

(() => {
  const tbody = document.getElementById('prices-tbody');
  const FIELD_NAMES = ['input_per_m', 'output_per_m', 'cache_write_per_m', 'cache_read_per_m'];

  // Baseline rate values (as last saved) and dirty tracking, keyed by row id
  // (a prefix can now own several rule rows, so id — not prefix — is unique).
  const originalById = {};
  const rowsById = new Map();
  const pricesById = new Map();
  const dirtyIds = new Set();

  // ── Shadow resolution sweep ──────────────────────────────────────────
  function ruleMatches(rule, x) {
    return rule.rule === 'under' ? x <= rule.rule_tokens : x > rule.rule_tokens;
  }

  /**
   * Among a prefix's rules that match x, the one whose rule_tokens is
   * numerically closest wins; an exact-distance tie (only possible with a
   * literal duplicate rule) goes to the more recently created one.
   * @param {object[]} rules - A prefix's price rules.
   * @param {number} x - Token count to resolve a winner for.
   * @returns {?object} The winning rule, or null if none match.
   */
  function pickWinner(rules, x) {
    let best = null;
    let bestDist = null;
    for (const r of rules) {
      if (!ruleMatches(r, x)) continue;
      const dist = Math.abs(r.rule_tokens - x);
      if (best === null || dist < bestDist || (dist === bestDist && r.created_at > best.created_at)) {
        best = r;
        bestDist = dist;
      }
    }
    return best;
  }

  /**
   * Every integer token count where the winner could change relative to the
   * integer just below it. Prompt token counts are always non-negative
   * integers, so the winner is piecewise-constant and only changes at:
   *   - 0, the start of the whole domain;
   *   - threshold+1 for every rule ("over N" starts matching at N+1, "under
   *     N" stops matching after N — either way that's the first token count
   *     where its match status differs from the one before it);
   *   - the integer crossover of every *opposite-direction* pair of rules
   *     whose ranges overlap, where the two rules' distances to x tie or
   *     flip (same-direction rules never cross — the rule closer to its own
   *     threshold is always at least as close everywhere both match).
   * An exact-distance tie (only possible when a pair's thresholds sum to an
   * even number) is isolated into its own single-token breakpoint, so the
   * recency-broken winner at that one token count doesn't get merged into
   * the unambiguous token counts on either side of it.
   * @param {object[]} rules - A prefix's price rules.
   * @returns {number[]} Sorted breakpoint token counts.
   */
  function computeBreakpoints(rules) {
    const breakpoints = new Set([0]);
    rules.forEach((r) => breakpoints.add(r.rule_tokens + 1));
    for (let i = 0; i < rules.length; i++) {
      for (let j = i + 1; j < rules.length; j++) {
        const a = rules[i];
        const b = rules[j];
        if (a.rule === b.rule) continue;
        const over = a.rule === 'over' ? a : b;
        const under = a.rule === 'under' ? a : b;
        if (over.rule_tokens >= under.rule_tokens) continue;
        const sum = over.rule_tokens + under.rule_tokens;
        if (sum % 2 === 0) {
          breakpoints.add(sum / 2);
          breakpoints.add(sum / 2 + 1);
        } else {
          breakpoints.add((sum + 1) / 2);
        }
      }
    }
    return Array.from(breakpoints).toSorted((x, y) => x - y);
  }

  /**
   * Walks the breakpoints and merges consecutive same-winner intervals into
   * segments described by real half-open [from, to) token bounds (to ===
   * Infinity for the unbounded tail). One sample at each interval's left
   * edge is enough — nothing changes strictly between two breakpoints.
   */
  function computeWinnerSegments(rules, bps) {
    const segments = [];
    bps.forEach((from, i) => {
      const to = i + 1 < bps.length ? bps[i + 1] : Infinity;
      const winner = pickWinner(rules, from);
      const winnerId = winner ? winner.id : null;
      const last = segments[segments.length - 1];
      if (last && last.winnerId === winnerId) {
        last.to = to;
      } else {
        segments.push({ winnerId, from, to });
      }
    });
    return segments;
  }

  /**
   * Formats a half-open [from, to) token range, matching ruleText's "> N" /
   * "≤ N" conventions; collapses to a single number for a one-token range.
   */
  function fmtRangeLabel(from, to) {
    if (to === Infinity) return `> ${fmtInt(from - 1)}`;
    const toIncl = to - 1;
    return from === toIncl ? fmtInt(from) : `${fmtInt(from)}–${fmtInt(toIncl)}`;
  }

  /**
   * For each rule in a prefix group, compares its nominal [from, to) range
   * against the winner segments to report whether another rule fully or
   * partially shadows it, and which rule(s)/range(s) it loses to.
   */
  function computeShadowInfo(rules) {
    const info = {};
    rules.forEach((r) => { info[r.id] = { status: 'none', losingTo: [] }; });
    if (rules.length <= 1) return info;

    const segments = computeWinnerSegments(rules, computeBreakpoints(rules));

    rules.forEach((r) => {
      // r's own nominal domain as a half-open [from, to) range, matching
      // the segments' convention above.
      const nomFrom = r.rule === 'under' ? 0 : r.rule_tokens + 1;
      const nomTo = r.rule === 'under' ? r.rule_tokens + 1 : Infinity;

      const overlapping = segments
        .map((s) => ({ winnerId: s.winnerId, from: Math.max(s.from, nomFrom), to: Math.min(s.to, nomTo) }))
        .filter((s) => s.from < s.to);

      const losing = overlapping.filter((s) => s.winnerId !== r.id);
      let status = 'none';
      if (losing.length > 0) status = losing.length === overlapping.length ? 'full' : 'partial';

      // Describe which other rule(s) win the shadowed part of this rule's
      // nominal range, for the tooltip.
      const losingTo = losing.map((s) => {
        const winner = rules.find((rr) => rr.id === s.winnerId);
        return winner ? { ruleLabel: ruleText(winner), range: fmtRangeLabel(s.from, s.to) } : null;
      }).filter(Boolean);

      info[r.id] = { status, losingTo };
    });
    return info;
  }

  /**
   * Builds the data-tip value as real HTML (app.js's tooltip sets
   * innerHTML directly), so this can lay out the losing ranges as a list
   * instead of a single run-on sentence.
   */
  function shadowTipHtml(shadow) {
    if (shadow.status === 'full') {
      return `<div class="font-semibold mb-1">Fully shadowed</div>
        <div>Another rule on this prefix wins for every token count this rule would otherwise match, so it never actually applies.</div>`;
    }
    const items = shadow.losingTo
      .map((l) => `<li class="tracking-wide leading-5 list-disc">${esc(l.ruleLabel)} wins instead, for tokens <span class="px-1 py-0.5 bg-gray-800 rounded">${esc(l.range)}</span></li>`)
      .join('');
    return `<div>This rule only wins outside the range(s) below:</div><ul class="mt-2 pl-4">${items}</ul>`;
  }

  function shadowBadgeHtml(shadow) {
    if (!shadow || shadow.status === 'none') return '';
    const isFull = shadow.status === 'full';
    const cls = isFull
      ? 'bg-gray-200 text-gray-600'
      : 'bg-amber-100 text-amber-700';
    const label = isFull ? 'shadowed' : 'partial';
    return `<span data-tip="${esc(shadowTipHtml(shadow))}" tabindex="0" class="ml-1.5 inline-block px-1.5 py-0.5 rounded text-[10px] font-medium ${cls} cursor-help align-middle">${label}</span>`;
  }

  /**
   * isRepeatPrefix dims the prefix text for every row after a group's
   * first, so consecutive rules sharing a prefix read as a visual group
   * without a dedicated header row taking up a whole row.
   */
  function buildRow(price, shadow, isRepeatPrefix) {
    return `<tr class="hover:bg-gray-50 transition-colors" data-id="${price.id}">
      <td class="prefix-cell px-4 py-2 font-mono text-xs transition-shadow duration-150">
        <span class="${isRepeatPrefix ? 'text-gray-400' : 'text-gray-900'}">${esc(price.model_prefix)}</span>
        <span class="dirty-dot hidden ml-1.5 inline-block h-1.5 w-1.5 rounded-full bg-amber-500 align-middle" title="Unsaved changes"></span>
      </td>
      <td class="px-4 py-2 capitalize">${esc(price.rule)}</td>
      <td class="px-4 py-2 text-right font-mono text-xs whitespace-nowrap">
        ${esc(ruleText(price))}${shadowBadgeHtml(shadow)}
      </td>
      <td class="px-4 py-2 text-right">
        <input type="number" step="0.01" min="0" name="input_per_m" value="${price.input_per_m}"
          class="border border-gray-300 rounded px-2 py-1 text-sm w-24 text-right focus:outline-none focus:ring-1 focus:ring-emerald-500">
      </td>
      <td class="px-4 py-2 text-right">
        <input type="number" step="0.01" min="0" name="output_per_m" value="${price.output_per_m}"
          class="border border-gray-300 rounded px-2 py-1 text-sm w-24 text-right focus:outline-none focus:ring-1 focus:ring-emerald-500">
      </td>
      <td class="px-4 py-2 text-right">
        <input type="number" step="0.01" min="0" name="cache_write_per_m" value="${price.cache_write_per_m}"
          class="border border-gray-300 rounded px-2 py-1 text-sm w-24 text-right focus:outline-none focus:ring-1 focus:ring-emerald-500">
      </td>
      <td class="px-4 py-2 text-right">
        <input type="number" step="0.01" min="0" name="cache_read_per_m" value="${price.cache_read_per_m}"
          class="border border-gray-300 rounded px-2 py-1 text-sm w-24 text-right focus:outline-none focus:ring-1 focus:ring-emerald-500">
      </td>
      <td class="px-4 py-2 text-gray-500 whitespace-nowrap updated-cell">${fmtTime(price.updated_at)}</td>
      <td class="px-4 py-2 text-right whitespace-nowrap">
        <button type="button" class="delete-btn text-sm text-gray-400 hover:text-red-600">Delete</button>
      </td>
      </tr>`;
  }

  function readRowValues(row) {
    const values = {};
    FIELD_NAMES.forEach((name) => {
      values[name] = parseFloat(row.querySelector(`[name="${name}"]`).value);
    });
    return values;
  }

  function readRowValuesAsStrings(row) {
    const values = {};
    FIELD_NAMES.forEach((name) => {
      values[name] = row.querySelector(`[name="${name}"]`).value;
    });
    return values;
  }

  async function deletePrice(id, label) {
    if (!confirm(`Delete price rule for ${label}?`)) return;
    await fetch(`/api/prices/${id}`, { method: 'DELETE' });
    loadPrices();
  }

  function updateRowDirtyVisual(row, dirty) {
    const cell = row.querySelector('.prefix-cell');
    if (cell) cell.style.boxShadow = dirty ? 'inset 3px 0 0 0 #f59e0b' : 'none';
    const dot = row.querySelector('.dirty-dot');
    if (dot) dot.classList.toggle('hidden', !dirty);
  }

  function setChangesMessage(text, isError) {
    const el = document.getElementById('price-changes-message');
    if (!el) return;
    el.textContent = text || '';
    el.classList.toggle('text-red-300', !!isError);
    el.classList.toggle('text-gray-300', !isError);
  }

  /**
   * Show/hide the sticky "unsaved changes" bar and update its count to match
   * the current size of dirtyIds.
   */
  function updateChangesBar() {
    const bar = document.getElementById('price-changes-bar');
    if (!bar) return;
    const count = dirtyIds.size;
    const countEl = document.getElementById('price-changes-count');
    if (countEl) countEl.textContent = `${count} unsaved change${count === 1 ? '' : 's'}`;

    if (count > 0) {
      bar.classList.remove('hidden');
      requestAnimationFrame(() => bar.classList.remove('translate-y-2', 'opacity-0'));
    } else if (!bar.classList.contains('hidden')) {
      bar.classList.add('translate-y-2', 'opacity-0');
      setTimeout(() => bar.classList.add('hidden'), 200);
    }
  }

  function refreshRowDirtyState(row) {
    const id = row.dataset.id;
    const original = originalById[id];
    if (!original) return;
    const dirty = FIELD_NAMES.some((name) => row.querySelector(`[name="${name}"]`).value !== original[name]);
    dirtyIds[dirty ? 'add' : 'delete'](id);
    updateRowDirtyVisual(row, dirty);
    setChangesMessage('', false);
    updateChangesBar();
  }

  function discardChanges() {
    dirtyIds.forEach((id) => {
      const row = rowsById.get(id);
      if (!row) return;
      const original = originalById[id];
      FIELD_NAMES.forEach((name) => {
        row.querySelector(`[name="${name}"]`).value = original[name];
      });
      updateRowDirtyVisual(row, false);
    });
    dirtyIds.clear();
    setChangesMessage('', false);
    updateChangesBar();
  }

  async function saveAllChanges() {
    if (dirtyIds.size === 0) return;
    const saveBtn = document.getElementById('price-changes-save');
    const discardBtn = document.getElementById('price-changes-discard');
    if (saveBtn) saveBtn.disabled = true;
    if (discardBtn) discardBtn.disabled = true;
    setChangesMessage('Saving…', false);

    const ids = Array.from(dirtyIds);
    const outcomes = await Promise.allSettled(ids.map(async (id) => {
      const row = rowsById.get(id);
      const res = await putJSON(`/api/prices/${id}`, readRowValues(row));
      if (!res.ok) throw new Error(await extractErrorMessage(res, `Failed to save row ${id}.`));
      return { id, row };
    }));

    let failedCount = 0;
    let lastError = '';
    outcomes.forEach((outcome) => {
      if (outcome.status === 'fulfilled') {
        const { id, row } = outcome.value;
        originalById[id] = readRowValuesAsStrings(row);
        dirtyIds.delete(id);
        updateRowDirtyVisual(row, false);
        const updatedCell = row.querySelector('.updated-cell');
        if (updatedCell) updatedCell.textContent = fmtTime(Math.floor(Date.now() / 1000));
      } else {
        failedCount += 1;
        lastError = outcome.reason.message;
      }
    });

    if (saveBtn) saveBtn.disabled = false;
    if (discardBtn) discardBtn.disabled = false;

    if (failedCount === 0) {
      setChangesMessage(`Saved ${ids.length} change${ids.length === 1 ? '' : 's'}.`, false);
      setTimeout(updateChangesBar, 900);
    } else {
      setChangesMessage(`Saved ${ids.length - failedCount}, ${failedCount} failed: ${lastError}`, true);
      updateChangesBar();
    }
  }

  async function loadPrices() {
    const res = await fetch('/api/prices');
    const prices = res.ok ? await res.json() : [];

    dirtyIds.clear();
    rowsById.clear();
    pricesById.clear();
    Object.keys(originalById).forEach((key) => delete originalById[key]);
    updateChangesBar();
    setChangesMessage('', false);

    if (!prices.length) {
      tbody.innerHTML = '<tr><td colspan="9" class="px-4 py-8 text-center text-gray-400">No model prices configured.</td></tr>';
      return;
    }

    prices.forEach((p) => pricesById.set(String(p.id), p));

    // Group by prefix, preserving the DB's `ORDER BY model_prefix, created_at`.
    const groups = Map.groupBy(prices, (p) => p.model_prefix);

    let html = '';
    groups.forEach((rules) => {
      const shadowByID = computeShadowInfo(rules);
      rules.forEach((p, idx) => { html += buildRow(p, shadowByID[p.id], idx > 0); });
    });
    tbody.innerHTML = html;

    tbody.querySelectorAll('tr[data-id]').forEach((row) => {
      const id = row.dataset.id;
      rowsById.set(id, row);
      originalById[id] = readRowValuesAsStrings(row);
    });
  }

  tbody.addEventListener('input', (e) => {
    const row = e.target.closest('tr[data-id]');
    if (row) refreshRowDirtyState(row);
  });

  tbody.addEventListener('click', (e) => {
    if (!e.target.closest('.delete-btn')) return;
    const row = e.target.closest('tr[data-id]');
    if (!row) return;
    const id = row.dataset.id;
    const price = pricesById.get(id);
    if (price) deletePrice(id, `${price.model_prefix} (${ruleText(price)})`);
  });

  document.getElementById('price-changes-save')?.addEventListener('click', saveAllChanges);
  document.getElementById('price-changes-discard')?.addEventListener('click', discardChanges);

  const setAddDialogMessage = makeDialogMessage('add-price-dialog-message', {
    error: ['text-red-700', 'bg-red-50', 'border-red-200'],
  });

  const addDialog = document.getElementById('add-price-dialog');
  const addForm = document.getElementById('add-price-form');
  if (addForm) {
    addForm.addEventListener('submit', async (e) => {
      e.preventDefault();
      const data = new FormData(addForm);
      const prefix = String(data.get('model_prefix') || '').trim();
      if (!prefix) return;
      const res = await postJSON('/api/prices', {
        model_prefix: prefix,
        rule: String(data.get('rule') || 'over'),
        rule_tokens: parseInt(data.get('rule_tokens'), 10) || 0,
        input_per_m: parseFloat(data.get('input_per_m')),
        output_per_m: parseFloat(data.get('output_per_m')),
        cache_write_per_m: parseFloat(data.get('cache_write_per_m')),
        cache_read_per_m: parseFloat(data.get('cache_read_per_m')),
      });
      if (!res.ok) {
        setAddDialogMessage('error', await extractErrorMessage(res, 'Failed to add price.'));
        return;
      }
      addDialog?.close();
      loadPrices();
    });
  }
  addDialog?.addEventListener('close', () => {
    setAddDialogMessage(null, '');
    addForm?.reset();
  });

  // ── Sync from LiteLLM ─────────────────────────────────────────────────
  const syncBtn = document.getElementById('sync-litellm-btn');
  const syncMessageEl = document.getElementById('litellm-sync-message');

  function setSyncMessage(text, isError) {
    if (!syncMessageEl) return;
    syncMessageEl.textContent = text || '';
    syncMessageEl.classList.toggle('text-red-600', !!isError);
    syncMessageEl.classList.toggle('text-gray-500', !isError);
  }

  // ── Auto-sync interval (persisted in the DB, not an env var, so it
  // applies live — see internal/pricesync.RunLoop) ──────────────────────
  const syncIntervalInput = document.getElementById('litellm-sync-interval');
  let lastSyncedAt = 0;

  function syncMessageWithLastSynced(text, isError) {
    const suffix = lastSyncedAt ? ` (last synced ${fmtTime(lastSyncedAt)})` : '';
    setSyncMessage(`${text}${suffix}`, isError);
  }

  async function loadSettings() {
    const res = await fetch('/api/settings');
    if (!res.ok || !syncIntervalInput) return;
    const settings = await res.json();
    syncIntervalInput.value = settings.litellm_sync_interval_minutes;
    lastSyncedAt = settings.litellm_last_synced_at;
    syncMessageWithLastSynced('', false);
  }

  syncIntervalInput?.addEventListener('change', async () => {
    const minutes = parseInt(syncIntervalInput.value, 10);
    if (Number.isNaN(minutes) || minutes < 0) {
      setSyncMessage('Enter a whole number of minutes (0 or more).', true);
      return;
    }
    const res = await putJSON('/api/settings', { litellm_sync_interval_minutes: minutes });
    if (!res.ok) {
      setSyncMessage(await extractErrorMessage(res, 'Failed to save interval.'), true);
      return;
    }
    setSyncMessage(minutes === 0 ? 'Auto-sync disabled.' : `Auto-syncing every ${minutes} min.`, false);
  });

  syncBtn?.addEventListener('click', async () => {
    syncBtn.disabled = true;
    setSyncMessage('Syncing…', false);
    try {
      const res = await postJSON('/api/prices/sync-litellm', {});
      if (!res.ok) {
        setSyncMessage(await extractErrorMessage(res, 'Sync failed.'), true);
        return;
      }
      const data = await res.json();
      lastSyncedAt = Math.floor(Date.now() / 1000);
      syncMessageWithLastSynced(`Synced ${data.models.length} model(s): ${data.created} created, ${data.updated} updated.`, false);
      loadPrices();
    } finally {
      syncBtn.disabled = false;
    }
  });

  loadPrices();
  loadSettings();
  initNavPolling();
})();
