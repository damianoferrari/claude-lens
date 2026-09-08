import { copyTextToClipboard, downloadTextFile, esc, estimateBytes, fmtBytes, initNavPolling } from './app.js';

'use strict';

(() => {
  let loaded = null; // { rawText: string, payload: object, sourceLabel: string }

  const dropzone = document.getElementById('dropzone');
  const fileInput = document.getElementById('dropzone-input');
  const errorEl = document.getElementById('analyze-error');
  const content = document.getElementById('analyze-content');

  function showError(message) {
    errorEl.textContent = message;
    errorEl.classList.remove('hidden');
  }

  function clearError() {
    errorEl.classList.add('hidden');
    errorEl.textContent = '';
  }

  function reset() {
    loaded = null;
    content.classList.add('hidden');
    content.classList.remove('flex');
    content.innerHTML = '';
    dropzone.classList.remove('hidden');
    fileInput.value = '';
    clearError();
  }

  /**
   * Parses raw JSON text as a request payload and renders it, or shows an
   * inline error when it isn't valid JSON.
   * @param {string} text - Raw JSON text, pasted, dropped, or uploaded.
   * @param {string} sourceLabel - Human-readable origin shown in the summary.
   */
  function loadFromText(text, sourceLabel) {
    clearError();
    let payload;
    try {
      payload = JSON.parse(text);
    } catch {
      showError("That doesn't look like valid JSON — paste or drop the exact raw request payload.");
      return;
    }
    if (typeof payload !== 'object' || payload === null || Array.isArray(payload)) {
      showError('Expected a JSON object (the request payload), not a list or plain value.');
      return;
    }
    loaded = { rawText: text, payload, sourceLabel };
    render();
  }

  function loadFromFile(file) {
    if (!/\.json$/i.test(file.name) && file.type !== 'application/json') {
      showError('Please provide a .json file.');
      return;
    }
    const reader = new FileReader();
    reader.onload = () => loadFromText(String(reader.result), file.name);
    reader.onerror = () => showError('Could not read that file.');
    reader.readAsText(file);
  }

  function render() {
    dropzone.classList.add('hidden');
    const { rawText, payload, sourceLabel } = loaded;
    const model = payload.model ?? '—';
    const isStreaming = !!payload.stream;

    content.innerHTML = `
      <div class="flex items-center justify-between">
        <h2 class="text-lg font-semibold">Loaded request</h2>
        <button type="button" id="analyze-reset" class="px-3 py-1.5 rounded-lg bg-gray-200 text-xs uppercase font-medium tracking-wide hover:bg-gray-300">Analyze another</button>
      </div>
      <div class="overflow-x-auto bg-white rounded-lg border border-gray-200">
        <table class="w-full text-xs">
          <tbody class="divide-y divide-gray-100">
            <tr class="*:p-3">
              <td class="font-medium uppercase tracking-wide text-gray-500">Source</td>
              <td class="font-mono text-gray-700 break-all text-right">${esc(sourceLabel)}</td>
            </tr>
            <tr class="*:p-3">
              <td class="font-medium uppercase tracking-wide text-gray-500">Model</td>
              <td class="font-mono text-gray-700 text-right">${esc(model)}</td>
            </tr>
            <tr class="*:p-3">
              <td class="font-medium uppercase tracking-wide text-gray-500">Was Streaming</td>
              <td class="font-mono text-gray-700 text-right">${isStreaming ? 'Yes' : 'No'}</td>
            </tr>
            <tr class="*:p-3">
              <td class="font-medium uppercase tracking-wide text-gray-500">
                <span class="inline-flex items-center gap-1.5">
                  Context size / Cost
                  <span class="text-xs text-gray-400" data-tip="Not shown: token counts and cost are computed by Anthropic and only exist in its response, which a request payload never carries.">ⓘ</span>
                </span>
              </td>
              <td class="font-mono text-gray-400 text-right">—</td>
            </tr>
          </tbody>
        </table>
      </div>
      <section class="flex flex-col gap-0.5">
        <div class="bg-white rounded-t-lg border border-gray-200 p-4 flex items-center justify-between">
          <div class="flex items-center gap-2">
            <h2 class="font-semibold uppercase tracking-wide">Request</h2>
            <button type="button" id="analyze-copy-raw" class="text-xs uppercase font-medium tracking-wide px-2 py-1 rounded bg-gray-200 hover:bg-gray-300">Copy Raw</button>
            <button type="button" id="analyze-download-raw" class="text-xs uppercase font-medium tracking-wide px-2 py-1 rounded bg-gray-200 hover:bg-gray-300">Download Raw</button>
          </div>
          <span class="text-xs text-gray-500 font-mono">${fmtBytes(estimateBytes(rawText))}</span>
        </div>
        <div class="bg-white rounded-b-lg border border-gray-200 p-4">
          <andypf-json-viewer
            id="analyze-json-viewer"
            indent="4"
            expanded="2"
            theme="google-light"
            show-data-types="false"
            show-toolbar="true"
            expand-icon-type="square"
            show-copy="false"
            show-size="true"
            preserve-expanded="false"
            expand-empty="false"
            copy-with-key="false"
          ></andypf-json-viewer>
        </div>
      </section>
    `;

    content.classList.remove('hidden');
    content.classList.add('flex');
    document.getElementById('analyze-json-viewer').data = rawText;
  }

  dropzone.addEventListener('click', () => fileInput.click());
  dropzone.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.preventDefault();
    fileInput.click();
  });
  dropzone.addEventListener('dragover', (e) => {
    e.preventDefault();
    dropzone.classList.add('border-emerald-400');
  });
  dropzone.addEventListener('dragleave', () => dropzone.classList.remove('border-emerald-400'));
  dropzone.addEventListener('drop', (e) => {
    e.preventDefault();
    dropzone.classList.remove('border-emerald-400');
    const file = e.dataTransfer.files?.[0];
    if (file) loadFromFile(file);
  });
  fileInput.addEventListener('change', () => {
    const file = fileInput.files?.[0];
    if (file) loadFromFile(file);
  });

  // Document-level rather than dropzone-focused, so paste works regardless
  // of keyboard focus. Only handled while nothing is loaded yet, so it never
  // fights with pasting into the JSON viewer's own toolbar controls.
  document.addEventListener('paste', (e) => {
    if (loaded) return;
    const text = e.clipboardData?.getData('text');
    if (!text) return;
    e.preventDefault();
    loadFromText(text, 'Pasted from clipboard');
  });

  document.addEventListener('click', async (e) => {
    if (e.target.closest('#analyze-reset')) {
      reset();
      return;
    }
    const copyTrigger = e.target.closest('#analyze-copy-raw');
    if (copyTrigger && loaded) {
      await copyTextToClipboard(copyTrigger, loaded.rawText, 'Copy Raw');
      return;
    }
    const downloadTrigger = e.target.closest('#analyze-download-raw');
    if (downloadTrigger && loaded) {
      downloadTextFile('analyzed-request.json', JSON.stringify(loaded.payload, null, 4));
    }
  });

  initNavPolling();
})();
