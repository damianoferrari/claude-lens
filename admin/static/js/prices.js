import { esc, extractErrorMessage, fmtCost, fmtTime, initNavPolling, makeDialogMessage, postJSON, putJSON } from './app.js';

'use strict';

(() => {
  const grid = document.getElementById('prices-grid');
  const pricesById = new Map();
  let editingId = null;

  const dialog = document.getElementById('price-dialog');
  const form = document.getElementById('price-form');
  const dialogTitle = document.getElementById('price-dialog-title');
  const submitBtn = document.getElementById('price-dialog-submit');
  const prefixInput = document.getElementById('price-model-prefix');

  const TIER_FIELDS = [
    ['input_per_m_above_200k', 'Input'],
    ['output_per_m_above_200k', 'Output'],
    ['cache_write_per_m_above_200k', 'Cache write'],
    ['cache_read_per_m_above_200k', 'Cache read'],
  ];

  function above200kSectionHtml(p) {
    const rows = TIER_FIELDS
      .filter(([field]) => p[field] != null)
      .map(([field, label]) => `<div class="flex justify-between"><span class="text-gray-500">${label} $/M</span><span class="font-mono text-gray-700">${fmtCost(p[field])}</span></div>`)
      .join('');
    if (!rows) return '';
    return `<div class="mt-3 pt-3 border-t border-gray-100">
      <p class="text-[10px] font-medium uppercase tracking-wide text-amber-600 mb-1.5">Above 200k tokens</p>
      <div class="text-xs space-y-1">${rows}</div>
    </div>`;
  }

  function buildCard(p) {
    return `<article class="bg-white rounded-lg border border-gray-200 p-4" data-id="${p.id}">
      <div class="flex items-start justify-between gap-2 mb-3">
        <p class="font-mono text-sm font-medium break-all">${esc(p.model_prefix)}</p>
        <div class="shrink-0 flex items-center gap-2">
          <button type="button" command="show-modal" commandfor="price-dialog" class="edit-btn text-sm text-gray-400 hover:text-gray-700">Edit</button>
          <button type="button" class="delete-btn text-sm text-gray-400 hover:text-red-600">Delete</button>
        </div>
      </div>
      <div class="text-sm grid grid-cols-2 gap-y-1">
        <div class="flex justify-between pr-2"><span class="text-gray-500">Input $/M</span><span class="font-mono text-gray-700">${fmtCost(p.input_per_m)}</span></div>
        <div class="flex justify-between pl-2"><span class="text-gray-500">Output $/M</span><span class="font-mono text-gray-700">${fmtCost(p.output_per_m)}</span></div>
        <div class="flex justify-between pr-2"><span class="text-gray-500">Cache write $/M</span><span class="font-mono text-gray-700">${fmtCost(p.cache_write_per_m)}</span></div>
        <div class="flex justify-between pl-2"><span class="text-gray-500">Cache read $/M</span><span class="font-mono text-gray-700">${fmtCost(p.cache_read_per_m)}</span></div>
      </div>
      ${above200kSectionHtml(p)}
      <div class="mt-5 flex justify-end">
        <p class="text-xs text-gray-400">Updated ${fmtTime(p.updated_at)}</p>
      </div>
    </article>`;
  }

  async function deletePrice(p) {
    if (!confirm(`Delete the price for ${p.model_prefix}?`)) return;
    await fetch(`/api/prices/${p.id}`, { method: 'DELETE' });
    loadPrices();
  }

  async function loadPrices() {
    const res = await fetch('/api/prices');
    const prices = res.ok ? await res.json() : [];

    pricesById.clear();
    if (!prices.length) {
      grid.innerHTML = '<p class="col-span-full text-center text-gray-400 py-8">No model prices configured.</p>';
      return;
    }

    prices.forEach((p) => pricesById.set(String(p.id), p));
    grid.innerHTML = prices.map(buildCard).join('');
  }

  grid.addEventListener('click', (e) => {
    const card = e.target.closest('article[data-id]');
    if (!card) return;
    const p = pricesById.get(card.dataset.id);
    if (!p) return;

    if (e.target.closest('.edit-btn')) {
      startEdit(p);
    } else if (e.target.closest('.delete-btn')) {
      deletePrice(p);
    }
  });

  // ── Add / Edit dialog wiring ────────────────────────────────────────
  const setDialogMessage = makeDialogMessage('price-dialog-message', {
    error: ['text-red-700', 'bg-red-50', 'border-red-200'],
  });

  function startEdit(p) {
    editingId = p.id;
    dialogTitle.textContent = 'Edit model price';
    submitBtn.textContent = 'Save changes';

    prefixInput.value = p.model_prefix;
    prefixInput.disabled = true;
    form.querySelector('[name="input_per_m"]').value = p.input_per_m;
    form.querySelector('[name="output_per_m"]').value = p.output_per_m;
    form.querySelector('[name="cache_write_per_m"]').value = p.cache_write_per_m;
    form.querySelector('[name="cache_read_per_m"]').value = p.cache_read_per_m;
    TIER_FIELDS.forEach(([field]) => {
      form.querySelector(`[name="${field}"]`).value = p[field] ?? '';
    });
  }

  function resetDialog() {
    editingId = null;
    dialogTitle.textContent = 'Add a model price';
    submitBtn.textContent = 'Add';
    prefixInput.disabled = false;
    setDialogMessage(null, '');
    form.reset();
  }
  dialog.addEventListener('close', resetDialog);

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const data = new FormData(form);
    const prefix = String(data.get('model_prefix') || '').trim();
    if (!prefix) return;

    const payload = {
      model_prefix: prefix,
      input_per_m: parseFloat(data.get('input_per_m')),
      output_per_m: parseFloat(data.get('output_per_m')),
      cache_write_per_m: parseFloat(data.get('cache_write_per_m')) || 0,
      cache_read_per_m: parseFloat(data.get('cache_read_per_m')) || 0,
    };
    TIER_FIELDS.forEach(([field]) => {
      const raw = String(data.get(field) || '').trim();
      payload[field] = raw ? parseFloat(raw) : null;
    });

    const res = editingId ? await putJSON(`/api/prices/${editingId}`, payload) : await postJSON('/api/prices', payload);
    if (!res.ok) {
      setDialogMessage('error', await extractErrorMessage(res, editingId ? 'Failed to update price.' : 'Failed to add price.'));
      return;
    }
    dialog.close();
    loadPrices();
  });

  // ── Sync from LiteLLM ─────────────────────────────────────────────────
  const syncMenu = document.getElementById('sync-litellm-menu');
  const syncToggle = document.getElementById('sync-litellm-toggle');
  const syncPanel = document.getElementById('sync-litellm-panel');
  const syncChevron = document.getElementById('sync-litellm-chevron');
  const syncBtn = document.getElementById('sync-litellm-btn');
  const syncMessageEl = document.getElementById('litellm-sync-message');

  function toggleSyncPanel(open = syncPanel.classList.contains('hidden')) {
    syncPanel.classList.toggle('hidden', !open);
    syncToggle.setAttribute('aria-expanded', String(open));
    syncChevron.classList.toggle('rotate-180', open);
  }

  syncToggle?.addEventListener('click', () => toggleSyncPanel());
  document.addEventListener('click', (e) => {
    if (!syncPanel.classList.contains('hidden') && !syncMenu.contains(e.target)) toggleSyncPanel(false);
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !syncPanel.classList.contains('hidden')) toggleSyncPanel(false);
  });

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

  // Grays out the manual button once we *know* this upstream can't sync —
  // driven by the last real attempt's outcome (manual or background, see
  // database.MarkLiteLLMSyncFailed), not a separate capability probe. The
  // interval input is deliberately never disabled here: RunLoop keeps
  // retrying on schedule regardless, so it self-heals the button the next
  // time settings are loaded, and the input stays a manual escape hatch
  // (set a short interval to force a fresh attempt soon) even while the
  // button itself is greyed out.
  function applyLiteLLMAvailability(errorMsg) {
    if (!syncBtn) return;
    syncBtn.disabled = !!errorMsg;
    if (errorMsg) {
      syncMessageWithLastSynced(`LiteLLM sync unavailable: ${errorMsg}`, true);
    } else {
      syncMessageWithLastSynced('', false);
    }
  }

  async function loadSettings() {
    const res = await fetch('/api/settings');
    if (!res.ok || !syncIntervalInput) return;
    const settings = await res.json();
    syncIntervalInput.value = settings.litellm_sync_interval_minutes;
    lastSyncedAt = settings.litellm_last_synced_at;
    applyLiteLLMAvailability(settings.litellm_last_sync_error);
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
    // Only a 502 (see admin.syncPricesFromLiteLLM) means "this upstream
    // isn't a LiteLLM proxy" — any other failure (e.g. a DB error) isn't a
    // capability signal, so the button stays usable for an immediate retry.
    let keepDisabled = false;
    try {
      const res = await postJSON('/api/prices/sync-litellm', {});
      if (!res.ok) {
        setSyncMessage(await extractErrorMessage(res, 'Sync failed.'), true);
        keepDisabled = res.status === 502;
        return;
      }
      const data = await res.json();
      lastSyncedAt = Math.floor(Date.now() / 1000);
      syncMessageWithLastSynced(`Synced ${data.models.length} model(s): ${data.created} created, ${data.updated} updated.`, false);
      loadPrices();
    } finally {
      syncBtn.disabled = keepDisabled;
    }
  });

  loadPrices();
  loadSettings();
  initNavPolling();
})();
