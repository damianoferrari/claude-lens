import { esc, extractErrorMessage, fmtCost, fmtCountdown, fmtTime, initNav, makeDialogMessage, pad, postJSON, progressBar, putJSON, fmtSessionId, fmtActivePeriod } from './app.js';

'use strict';

(() => {
  const tbody = document.getElementById('limiters-tbody');
  const limitersById = new Map();
  let editingId = null;

  const ALIGNED_COMBOS = { minutes: [60], hours: [1, 24], days: [1, 7], months: [1] };

  const dialog = document.getElementById('limiter-dialog');
  const form = document.getElementById('limiter-form');
  const dialogTitle = document.getElementById('limiter-dialog-title');
  const submitBtn = document.getElementById('limiter-dialog-submit');
  const sessionIDInput = document.getElementById('limiter-session-id');
  const limitAmountInput = document.getElementById('limiter-limit-amount');
  const refreshValueInput = document.getElementById('limiter-refresh-value');
  const refreshUnitSelect = document.getElementById('limiter-refresh-unit');
  const alignedCheckbox = document.getElementById('limiter-refresh-aligned');
  const alwaysActiveCheckbox = document.getElementById('limiter-always-active');
  const startHourSelect = document.getElementById('limiter-active-start-hour');
  const endHourSelect = document.getElementById('limiter-active-end-hour');
  const alertThresholdPctInput = document.getElementById('limiter-alert-threshold-pct');
  const alertRequestCostUsdInput = document.getElementById('limiter-alert-request-cost-usd');
  const slackWebhookUrlInput = document.getElementById('limiter-slack-webhook-url');

  function populateHourSelect(select) {
    select.innerHTML = Array.from({ length: 24 }, (_, h) => `<option value="${h}">${pad(h)}:00</option>`).join('');
  }
  populateHourSelect(startHourSelect);
  populateHourSelect(endHourSelect);
  startHourSelect.value = '0';
  endHourSelect.value = '23';

  function unitLabel(value, unit) {
    return value === 1 ? unit.slice(0, -1) : unit;
  }

  function fmtRefresh(l) {
    const base = `${l.refresh_value} ${unitLabel(l.refresh_value, l.refresh_unit)}`;
    return l.refresh_aligned ? `${base} (aligned)` : base;
  }

  function scopeBadge(l) {
    const badge = !l.session_id
      ? '<span class="px-1.5 py-0.5 bg-blue-50 text-blue-700 rounded font-medium">Global</span>'
      : `<span class="px-1.5 py-0.5 bg-emerald-50 text-emerald-700 rounded font-medium">${esc(fmtSessionId(l.session_id))}</span>`;
    return `${badge}${alertBadge(l)}`;
  }

  // Small bell indicator shown next to the scope badge when either Slack
  // alert is armed for this limiter, with the details in a tooltip.
  function alertBadge(l) {
    if (!l.alert_threshold_pct && !l.alert_request_cost_usd) return '';
    const lines = [];
    if (l.alert_threshold_pct) lines.push(`At ${l.alert_threshold_pct}% of budget${l.alert_sent ? ' (already sent this window)' : ''}`);
    if (l.alert_request_cost_usd) lines.push(`On a single request over ${fmtCost(l.alert_request_cost_usd)}`);
    const tip = `Slack alert:\n${lines.join('\n')}`;
    return ` <span data-tip="${esc(tip)}" class="text-amber-500 cursor-default" aria-label="${esc(tip)}">🔔</span>`;
  }

  function statusToggle(l) {
    return `<button type="button" class="toggle-active-btn relative inline-flex h-5 w-9 items-center rounded-full transition-colors ${l.is_active ? 'bg-emerald-600' : 'bg-gray-300'}"
      data-id="${l.id}" data-active="${l.is_active}" aria-pressed="${l.is_active}" aria-label="Toggle limiter active">
      <span class="inline-block h-3.5 w-3.5 transform rounded-full bg-white transition-transform ${l.is_active ? 'translate-x-5' : 'translate-x-0.5'}"></span>
    </button>`;
  }

  function buildRow(l) {
    return `<tr class="hover:bg-gray-50 transition-colors" data-id="${l.id}">
      <td class="px-4 py-2">${scopeBadge(l)}</td>
      <td class="px-4 py-2">${fmtCost(l.limit_amount)}</td>
      <td class="px-4 py-2 w-40">${progressBar(l)}</td>
      <td class="px-4 py-2 whitespace-nowrap">
        <div>${esc(fmtRefresh(l))}</div>
        <div class="text-xs text-gray-400" data-countdown="${l.id}">${esc(fmtCountdown(l.within_active_period && l.is_active ? l.next_refresh_at : null))}</div>
      </td>
      <td class="px-4 py-2 whitespace-nowrap">${fmtActivePeriod(l)}</td>
      <td class="px-4 py-2">${statusToggle(l)}</td>
      <td class="px-4 py-2 text-gray-500 whitespace-nowrap">${fmtTime(l.updated_at)}</td>
      <td class="px-4 py-2 text-right whitespace-nowrap">
        <button type="button" command="show-modal" commandfor="limiter-dialog" class="edit-btn text-sm text-gray-400 hover:text-gray-700 mr-3">Edit</button>
        <button type="button" class="delete-btn text-sm text-gray-400 hover:text-red-600">Delete</button>
      </td>
    </tr>`;
  }

  async function loadLimiters() {
    const res = await fetch('/api/limiters');
    const limiters = res.ok ? await res.json() : [];

    limitersById.clear();
    if (!limiters.length) {
      tbody.innerHTML = '<tr><td colspan="8" class="px-4 py-8 text-center text-gray-400">No limiters configured.</td></tr>';
      return;
    }

    limiters.forEach((l) => limitersById.set(String(l.id), l));
    tbody.innerHTML = limiters.map(buildRow).join('');
  }

  // Recomputes just the countdown text every 30s from already-fetched data,
  // so it visibly ticks down between actual data changes without a refetch
  // or a full-table re-render.
  function tickCountdowns() {
    tbody.querySelectorAll('[data-countdown]').forEach((el) => {
      const l = limitersById.get(el.dataset.countdown);
      if (l) el.textContent = fmtCountdown(l.next_refresh_at);
    });
  }

  async function toggleActive(l) {
    await fetch(`/api/limiters/${l.id}/active`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ is_active: !l.is_active }),
    });
    loadLimiters();
  }

  async function deleteLimiter(l) {
    const scope = l.session_id ? `session ${l.session_id}` : 'global';
    if (!confirm(`Delete the ${scope} limiter (${fmtCost(l.limit_amount)})?`)) return;
    await fetch(`/api/limiters/${l.id}`, { method: 'DELETE' });
    loadLimiters();
  }

  tbody.addEventListener('click', (e) => {
    const row = e.target.closest('tr[data-id]');
    if (!row) return;
    const l = limitersById.get(row.dataset.id);
    if (!l) return;

    if (e.target.closest('.toggle-active-btn')) {
      toggleActive(l);
    } else if (e.target.closest('.edit-btn')) {
      startEdit(l);
    } else if (e.target.closest('.delete-btn')) {
      deleteLimiter(l);
    }
  });

  // ── Aligned-refresh availability ────────────────────────────────────
  function updateAlignedAvailability() {
    const value = parseInt(refreshValueInput.value, 10);
    const unit = refreshUnitSelect.value;
    const supported = (ALIGNED_COMBOS[unit] || []).includes(value);
    alignedCheckbox.disabled = !supported;
    if (!supported) alignedCheckbox.checked = false;
  }
  refreshValueInput.addEventListener('input', updateAlignedAvailability);
  refreshUnitSelect.addEventListener('change', updateAlignedAvailability);

  // ── Always-active toggle ────────────────────────────────────────────
  function updateActivePeriodInputs() {
    const disabled = alwaysActiveCheckbox.checked;
    startHourSelect.disabled = disabled;
    endHourSelect.disabled = disabled;
  }
  alwaysActiveCheckbox.addEventListener('change', updateActivePeriodInputs);

  // ── Dialog message ──────────────────────────────────────────────────
  const setDialogMessage = makeDialogMessage('limiter-dialog-message', {
    error: ['text-red-700', 'bg-red-50', 'border-red-200'],
  });

  // ── Add / Edit dialog wiring ────────────────────────────────────────
  function startEdit(l) {
    editingId = l.id;
    dialogTitle.textContent = 'Edit limiter';
    submitBtn.textContent = 'Save changes';

    sessionIDInput.value = l.session_id;
    limitAmountInput.value = l.limit_amount;
    refreshValueInput.value = l.refresh_value;
    refreshUnitSelect.value = l.refresh_unit;
    updateAlignedAvailability();
    alignedCheckbox.checked = l.refresh_aligned && !alignedCheckbox.disabled;

    alwaysActiveCheckbox.checked = l.active_start_hour == null;
    startHourSelect.value = String(l.active_start_hour ?? 0);
    endHourSelect.value = String(l.active_end_hour ?? 23);
    updateActivePeriodInputs();

    alertThresholdPctInput.value = l.alert_threshold_pct ?? '';
    alertRequestCostUsdInput.value = l.alert_request_cost_usd ?? '';
    slackWebhookUrlInput.value = l.slack_webhook_url ?? '';
  }

  function resetDialog() {
    editingId = null;
    dialogTitle.textContent = 'Add a limiter';
    submitBtn.textContent = 'Add';
    setDialogMessage(null, '');
    form.reset();
    updateAlignedAvailability();
    updateActivePeriodInputs();
  }
  dialog.addEventListener('close', resetDialog);

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const data = new FormData(form);
    const alwaysActive = alwaysActiveCheckbox.checked;
    const alertThresholdPct = String(data.get('alert_threshold_pct') || '').trim();
    const alertRequestCostUsd = String(data.get('alert_request_cost_usd') || '').trim();

    const payload = {
      session_id: String(data.get('session_id') || '').trim(),
      limit_amount: parseFloat(data.get('limit_amount')),
      refresh_value: parseInt(data.get('refresh_value'), 10),
      refresh_unit: String(data.get('refresh_unit')),
      refresh_aligned: alignedCheckbox.checked && !alignedCheckbox.disabled,
      active_start_hour: alwaysActive ? null : parseInt(data.get('active_start_hour'), 10),
      active_end_hour: alwaysActive ? null : parseInt(data.get('active_end_hour'), 10),
      slack_webhook_url: String(data.get('slack_webhook_url') || '').trim(),
      alert_threshold_pct: alertThresholdPct ? parseInt(alertThresholdPct, 10) : null,
      alert_request_cost_usd: alertRequestCostUsd ? parseFloat(alertRequestCostUsd) : null,
    };

    const res = editingId ? await putJSON(`/api/limiters/${editingId}`, payload) : await postJSON('/api/limiters', payload);
    if (!res.ok) {
      setDialogMessage('error', await extractErrorMessage(res, editingId ? 'Failed to update limiter.' : 'Failed to add limiter.'));
      return;
    }
    dialog.close();
    loadLimiters();
  });

  updateAlignedAvailability();
  updateActivePeriodInputs();
  loadLimiters();
  initNav(null, { onLimitersChanged: loadLimiters });
  setInterval(tickCountdowns, 30000);
})();
