import { debounce, esc, estimateBytes, fmtBytes, fmtCost, fmtInt, fmtTime, hashStr, initNavPolling, prettyJSON, randomId, ruleText, valueToStr } from './app.js';

'use strict';

(() => {
  const exchangeId = window.location.pathname.split('/').filter(Boolean).pop();
  const tabSize = 4;
  let derivedExchange = null;
  let messageOwnerIndex = new Map();

  /**
   * Goes back to the previous page if the referrer is same-origin, otherwise
   * does nothing.
   * @param {MouseEvent} e - Click event on the back link.
   */
  function goBackToExchanges(e) {
    const referrer = document.referrer;
    const sameOriginReferrer = referrer && new URL(referrer).origin === window.location.origin;
    if (!sameOriginReferrer) return;
    e.preventDefault();
    if (window.history.length > 1) window.history.back();
    else window.location.href = referrer;
  }
  const backLink = document.getElementById('back-link');
  if (backLink) backLink.addEventListener('click', goBackToExchanges);
  const notFoundBackLink = document.getElementById('not-found-back-link');
  if (notFoundBackLink) notFoundBackLink.addEventListener('click', goBackToExchanges);

  /**
   * Recursively strips `cache_control` breakpoints from a parsed message.
   * Anthropic clients move this ephemeral marker onto whichever message is
   * currently last in a request, so the same logical message hashes
   * differently depending on whether it's the newest turn or buried in a
   * later request's history — stripping it makes the hash a stable identity
   * for "same message" regardless of where it appears.
   * @param {*} value - Parsed JSON value (message, array, or scalar).
   * @returns {*} A deep copy of value with every `cache_control` key removed.
   */
  function stripCacheControl(value) {
    if (Array.isArray(value)) return value.map(stripCacheControl);
    if (value && typeof value === 'object') {
      return Object.fromEntries(
        Object.entries(value)
          .filter(([key]) => key !== 'cache_control')
          .map(([key, val]) => [key, stripCacheControl(val)])
      );
    }
    return value;
  }

  /**
   * Adds index/number/hash/content_text/content_bytes derived fields to each
   * input message, used by renderInputMessages and the message dialog.
   * @param {object[]} inputMessages - Raw input messages parsed from the exchange.
   * @returns {Promise<object[]>} The same array, mutated in place with derived fields.
   */
  async function deriveInputMessages(inputMessages) {
    await Promise.all(inputMessages.map(async (msg, index) => {
      // Hash before stamping index/number: those are this array's own
      // positions, not part of the message, and must not affect the hash
      // used to match this message against another exchange's own prompt.
      msg.hash = await hashStr(valueToStr(stripCacheControl(msg), 0));
      msg.index = index;
      msg.number = index + 1;
      msg.content_text = valueToStr(msg?.content, tabSize);
      msg.content_bytes = estimateBytes(msg.content_text);
    }));
    return inputMessages;
  }

  /**
   * Adds byte-size fields, parsed+derived input_messages, and a convenience
   * `input` (the last input message) to a raw exchange fetched from the API.
   * @param {?object} exchange - Raw exchange from the API, or null.
   * @returns {Promise<?object>} The same object, mutated in place with derived fields.
   */
  async function deriveExchange(exchange) {
    if (!exchange) return exchange;
    exchange.raw_request_bytes = estimateBytes(exchange.raw_request);
    exchange.raw_response_bytes = estimateBytes(exchange.raw_response);
    exchange.input_messages_bytes = estimateBytes(exchange.input_messages);
    exchange.input_messages = await deriveInputMessages(JSON.parse(exchange.input_messages || '[]'));
    exchange.input_messages_length = exchange.input_messages.length;
    exchange.input = exchange.input_messages.at(-1) || null;
    exchange.output_bytes = estimateBytes(exchange.output_text);
    return exchange;
  }

  /**
   * Hashes the last input message of every other exchange in the same
   * session, so a message in this exchange's own input_messages can be
   * recognized as another exchange's own prompt. On a hash collision between
   * siblings, the oldest exchange (the true origin) wins.
   * @param {string} exchangeId - Id of the exchange whose session siblings to index.
   * @returns {Promise<Map<string, number>>} Map of message hash to owning exchange id.
   */
  async function buildMessageOwnerIndex(exchangeId) {
    const res = await fetch(`/api/exchanges/${exchangeId}/session-inputs`);
    if (!res.ok) return new Map();
    const rows = await res.json();
    const hashes = await Promise.all(rows.map(async (row) => {
      if (!row.last_input_message) return null;
      return hashStr(valueToStr(stripCacheControl(JSON.parse(row.last_input_message)), 0));
    }));
    const index = new Map();
    rows.forEach((row, i) => {
      if (hashes[i] && !index.has(hashes[i])) index.set(hashes[i], row.id);
    });
    return index;
  }

  function renderInputMessages(inputMessages, messageOwnerIndex) {
    let html = '<ol class="relative border-s border-default flex flex-col gap-16" role="list" e-input-messages-content>';

    // Render from end to start
    for (let i = inputMessages.length - 1; i >= 0; i--) {
      const msg = inputMessages[i];
      // The last message is this exchange's own prompt, not owned by anyone else.
      const ownerId = i < inputMessages.length - 1 ? messageOwnerIndex.get(msg.hash) : undefined;
      const hashMarkup = ownerId
        ? `<a href="/exchanges/${ownerId}" class="font-mono text-xs text-gray-500 hover:text-emerald-500"><span class="underline mr-0.5">${msg.hash}</span>⧉</a>`
        : `<span class="font-mono text-xs text-gray-500 hover:text-emerald-500">${msg.hash}</span>`;
      html += `
        <li class="ms-10 flex items-center overflow-x-hidden">
          <span class="absolute bg-white w-20 flex items-center justify-center text-xs text-gray-500 font-mono mt-1.5 -start-10 py-2">${msg.number}</span>
          <div class="flex flex-col gap-2 border-l-2 border-gray-200 rounded-lg px-2">
            ${hashMarkup}
            <div class="flex items-center gap-2">
              <span class="bg-gray-50 border border-gray-200 text-heading text-xs font-medium px-1.5 py-0.5 rounded w-fit">${msg.role || '—'}</span>
              <span class="bg-gray-50 border border-gray-200 text-heading text-[.7rem] font-medium px-1.5 py-0.5 rounded w-fit">${fmtBytes(msg.content_bytes)}</span>
            </div>
            <div class="font-mono text-gray-700 text-xs text-nowrap cursor-pointer hover:text-emerald-500" open-message-dialog="${msg.index}">${esc(msg.content_text || '—')}</div>
          </div>
        </li>
      `;
    }

    html += '</ol>';
    return html;
  }

  function render(exchange) {
    document.getElementById('page-title').textContent = `Exchange #${exchange.id} – claude-lens Admin`;

    const content = document.getElementById('exchange-content');
    let html = `
      <h1 class="text-xl font-semibold">Exchange #${exchange.id}</h1>
      <section class="flex flex-col gap-3">
        <div class="bg-white rounded-lg border border-gray-200 p-5 grid grid-cols-2 sm:grid-cols-3 gap-x-8 gap-y-4 text-sm">
          <div>
            <p class="font-medium uppercase tracking-wide underline">Session ID</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${esc(exchange.session_id)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Session name</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${esc(exchange.session_name || '—')}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Time</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${fmtTime(exchange.timestamp)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Path</p>
            <p class="mt-1 font-mono text-gray-700 text-sm">${esc(exchange.path)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Model</p>
            <p class="mt-1 font-mono text-gray-700 text-sm">${esc(exchange.model || '—')}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Cost</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${fmtCost(exchange.cost)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Was Streaming</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${exchange.is_streaming ? 'Yes' : 'No'}</p>
          </div>
        </div>
        <div class="bg-white rounded-lg border border-gray-200 p-5 grid grid-cols-2 sm:grid-cols-3 gap-x-8 gap-y-4 text-sm">
          <div>
            <p class="font-medium uppercase tracking-wide underline">Input tokens</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${fmtInt(exchange.input_tokens)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Output tokens</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${fmtInt(exchange.output_tokens)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Cache creation tokens</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${fmtInt(exchange.cache_creation_tokens)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Cache read tokens</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${fmtInt(exchange.cache_read_tokens)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Context size</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${fmtBytes(exchange.input_messages_bytes)}</p>
          </div>
        </div>
        <div class="bg-white rounded-lg border border-gray-200 p-5 grid grid-cols-2 sm:grid-cols-3 gap-x-8 gap-y-4 text-sm">
          <div class="col-span-2 sm:col-span-3 flex items-center gap-1.5">
            <p class="font-medium uppercase tracking-wide underline">Matched price rule</p>
            <span class="text-xs text-gray-400" data-tip="Captured when this exchange was saved — a permanent snapshot of what was actually charged, even if the rule is edited or deleted later.">ⓘ</span>
          </div>
          ${exchange.matched_price ? `
          <div>
            <p class="font-medium uppercase tracking-wide underline">Prefix</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${esc(exchange.matched_price.model_prefix)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Rule</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${esc(ruleText(exchange.matched_price))}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Input $/M</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${fmtCost(exchange.matched_price.input_per_m)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Output $/M</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${fmtCost(exchange.matched_price.output_per_m)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Cache write $/M</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${fmtCost(exchange.matched_price.cache_write_per_m)}</p>
          </div>
          <div>
            <p class="font-medium uppercase tracking-wide underline">Cache read $/M</p>
            <p class="mt-1 font-mono text-gray-700 text-sm break-all">${fmtCost(exchange.matched_price.cache_read_per_m)}</p>
          </div>
          ` : `
          <div class="col-span-2 sm:col-span-3">
            <p class="text-gray-400 text-sm">No price rule matched this exchange's model/token count when it was saved.</p>
          </div>
          `}
        </div>
      </section>
    `;

    if (exchange.input) {
      const id = randomId();
      html += `
        <section class="flex flex-col gap-0.5">
          <div class="bg-white rounded-t-lg border border-gray-200 p-4 flex items-center justify-between cursor-pointer" trigger-content-e="${id}">
            <div class="flex items-center gap-2">
              <h2 class="font-semibold uppercase tracking-wide">Input</h2>
              <div class="flex flex-col items-start gap-2 border-l-2 border-gray-200 rounded-lg p-2">
                <span class="font-mono text-xs text-gray-500" data-tip="Position of this prompt in the input messages (context)">${fmtInt(exchange.input.number)} of ${fmtInt(exchange.input_messages_length)}</span>
                <span class="font-mono text-xs text-gray-500 hover:text-emerald-500" data-tip="Hash of this prompt in the input messages (context)">${exchange.input.hash}</span>
              </div>
            </div>
            <span class="text-xs text-gray-500 font-mono">${fmtBytes(exchange.input.content_bytes)}</span>
          </div>
          <div class="bg-white rounded-b-lg border border-gray-200 p-4 text-xs text-gray-700 font-mono whitespace-pre-wrap leading-relaxed max-h-80 overflow-y-auto" e-content-id="${id}">${esc(exchange.input.content_text)}</div>
        </section>
      `;
    }

    if (exchange.output_text) {
      const id = randomId();
      html += `
        <section class="flex flex-col gap-0.5">
          <div class="bg-white rounded-t-lg border border-gray-200 p-4 flex items-center justify-between cursor-pointer" trigger-content-e="${id}">
            <h2 class="font-semibold uppercase tracking-wide">Output</h2>
            <span class="text-xs text-gray-500 font-mono">${fmtBytes(exchange.output_bytes)}</span>
          </div>
          <div class="bg-white rounded-b-lg border border-gray-200 p-4 text-xs text-gray-700 font-mono whitespace-pre-wrap leading-relaxed max-h-80 overflow-y-auto" e-content-id="${id}">${esc(exchange.output_text)}</div>
        </section>
      `;
    }

    if (exchange.input_messages.length > 0) {
      const id = randomId();
      html += `
        <section class="flex flex-col gap-0.5">
          <div class="bg-white rounded-t-lg border border-gray-200 p-4 flex items-center justify-between cursor-pointer" trigger-content-e="${id}">
            <h2 class="font-semibold uppercase tracking-wide">Context Messages</h2>
            <span class="text-xs text-gray-500 font-mono">${fmtBytes(exchange.input_messages_bytes)}</span>
          </div>
          <div class="flex flex-col gap-0.5" e-content-id="${id}">
            <div class="bg-white border border-gray-200 p-4">
              <input type="search" id="input-messages-search" placeholder="Search…" class="w-full px-3 py-2 border border-gray-300 rounded-md focus:outline-none focus:ring-2 focus:ring-emerald-500">
            </div>
            <div class="bg-white rounded-b-lg border border-gray-200 py-8 px-10 max-h-[750px] overflow-y-auto">${renderInputMessages(exchange.input_messages, messageOwnerIndex)}</div>
          </div>
        </section>
      `;
    }

    if (exchange.raw_request) {
      const id = randomId();
      html += `
        <section class="flex flex-col gap-0.5">
          <div class="bg-white rounded-t-lg border border-gray-200 p-4 flex items-center justify-between cursor-pointer rounded-b-lg border-dashed border-2" trigger-content-e="${id}">
            <h2 class="font-semibold uppercase tracking-wide">Raw Request</h2>
            <span class="text-xs text-gray-500 font-mono">${fmtBytes(exchange.raw_request_bytes)}</span>
          </div>
          <div class="bg-white rounded-b-lg border border-gray-200 p-4 text-xs text-gray-700 font-mono whitespace-pre-wrap leading-relaxed max-h-80 overflow-y-auto hidden" e-content-id="${id}">${esc(prettyJSON(exchange.raw_request, tabSize))}</div>
        </section>
      `;
    }

    if (exchange.raw_response) {
      const id = randomId();
      html += `
        <section class="flex flex-col gap-0.5">
          <div class="bg-white rounded-t-lg border border-gray-200 p-4 flex items-center justify-between cursor-pointer rounded-b-lg border-dashed border-2" trigger-content-e="${id}">
            <h2 class="font-semibold uppercase tracking-wide">Raw Response</h2>
            <span class="text-xs text-gray-500 font-mono">${fmtBytes(exchange.raw_response_bytes)}</span>
          </div>
          <div class="bg-white rounded-b-lg border border-gray-200 p-4 text-xs text-gray-700 font-mono whitespace-pre-wrap leading-relaxed max-h-80 overflow-y-auto hidden" e-content-id="${id}">${esc(exchange.raw_response)}</div>
        </section>
      `;
    }

    content.innerHTML = html;
  }

  function toggleContent(e) {
    const trigger = e.target.closest('[trigger-content-e]');
    if (!trigger) return;
    const id = trigger.getAttribute('trigger-content-e');
    const content = document.querySelector(`[e-content-id="${id}"]`);
    if (!content) return;
    content.classList.toggle('hidden');
    trigger.classList.toggle('rounded-b-lg');
    trigger.classList.toggle('border-dashed');
    trigger.classList.toggle('border-2');
  }

  function showNotFound() {
    document.getElementById('exchange-content').classList.add('hidden');
    document.getElementById('not-found-content').classList.remove('hidden');
    document.getElementById('not-found-message').textContent = exchangeId
      ? `Exchange #${exchangeId} was not found.`
      : 'Page not found.';
  }

  async function load() {
    if (!exchangeId || !/^\d+$/.test(exchangeId)) {
      showNotFound();
      return;
    }

    const [res, ownerIndex] = await Promise.all([
      fetch(`/api/exchanges/${exchangeId}`),
      buildMessageOwnerIndex(exchangeId),
    ]);
    if (!res.ok) {
      showNotFound();
      return;
    }

    messageOwnerIndex = ownerIndex;
    derivedExchange = await deriveExchange(await res.json());

    render(derivedExchange);
  }

  document.addEventListener('click', toggleContent);
  document.addEventListener('click', (e) => {
    const trigger = e.target.closest('[open-message-dialog]');
    if (!trigger) return;
    const msgIndex = trigger.getAttribute('open-message-dialog');
    const msg = derivedExchange.input_messages[msgIndex];
    if (!msg || !msg.content_text) return;
    const dialog = document.getElementById('message-dialog');
    const dialogContent = document.getElementById('message-dialog-content');
    dialogContent.textContent = msg.content_text;
    document.getElementById('message-dialog-search').value = '';
    dialog.showModal();
  });
  document.addEventListener('input', debounce((e) => {
    const inputMessageSearch = e.target.closest('#input-messages-search');
    if (!inputMessageSearch) {
      const inputMessagesContainer = document.querySelector('[e-input-messages-content]');
      if (inputMessagesContainer) {
        inputMessagesContainer.scrollTo({ top: 0, left: 0, behavior: 'instant' });
      }
      return;
    }
    const searchTerm = inputMessageSearch.value;
    const inputMessagesContainer = document.querySelector('[e-input-messages-content]');
    if (!inputMessagesContainer) return;
    const messages = inputMessagesContainer.querySelectorAll('li');

    let firstMatch = null;
    messages.forEach((msg) => {
      const visible = msg.textContent.includes(searchTerm);
      msg.style.display = visible ? '' : 'none';
      if (visible && !firstMatch) firstMatch = msg;
    });

    if (firstMatch) firstMatch.scrollIntoView({ behavior: 'instant', block: 'nearest' });
    else inputMessagesContainer.scrollTo({ top: 0, left: 0, behavior: 'instant' });
  }));

  document.addEventListener('DOMContentLoaded', () => {
    const messageDialogSearch = document.getElementById('message-dialog-search');

    // Highlight search terms in the message dialog content as the user types
    messageDialogSearch.addEventListener('input', debounce((e) => {
      const dialogContent = document.getElementById('message-dialog-content');
      const searchTerm = e.target.value;
      const originalText = dialogContent.textContent;

      if (!searchTerm) {
        dialogContent.textContent = originalText;
        dialogContent.scrollTo({ top: 0, left: 0, behavior: 'instant' });
        return;
      }

      dialogContent.innerHTML = originalText.replace(
        new RegExp(`(${searchTerm})`, 'gi'),
        '<mark>$1</mark>'
      );

      const firstMark = dialogContent.querySelector('mark');
      if (firstMark) firstMark.scrollIntoView({ behavior: 'instant', block: 'nearest' });
      else dialogContent.scrollTo({ top: 0, left: 0, behavior: 'instant' });
    }));
  });

  load();
  initNavPolling();
})();
