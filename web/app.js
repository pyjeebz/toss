// Toss -- M1 client.
//
// Paste to send, EventSource to receive, tap to copy back. Rooms live at
// /r/<room> and are remembered in localStorage; a short code or a QR scan
// brings another device into the same room.
//
// Content is still plaintext on the wire until M3. The server already treats it
// as opaque, and /api/pair already carries an opaque payload for the wrapped
// room key, so M3 lands here and nowhere else.

(() => {
  'use strict';

  const $ = (role, root = document) => root.querySelector(`[data-role="${role}"]`);

  const els = {
    list: $('list'),
    empty: $('empty'),
    clear: $('clear'),
    status: $('status'),
    statusLabel: $('status-label'),
    live: $('live'),
    template: $('scrap-template'),
    pair: $('pair'),
    sheet: $('pair-sheet'),
    pairClose: $('pair-close'),
    pairQR: $('pair-qr'),
    pairCode: $('pair-code'),
    pairCountdown: $('pair-countdown'),
    joinForm: $('pair-join-form'),
    joinInput: $('pair-join-input'),
    joinError: $('pair-join-error'),
  };

  // localStorage holds the room ID, and from M3 the key. Never item content.
  const STORAGE_ROOM = 'toss.room';

  // id -> { item, el }. The single source of truth for what is on screen, and
  // what makes duplicate delivery (stream replay overlapping a POST response)
  // a non-event.
  const rendered = new Map();

  // Correlation key -> temp id, for sends that have not come back yet. Lets an
  // item arriving on the stream be recognised as the echo of our own optimistic
  // render rather than a new arrival, whichever of the two lands first.
  const inflight = new Map();

  // In M1 the only handle both sides share before the POST returns is the
  // content itself. From M3 it is the IV: the client generates that too, it is
  // unique per item, and it survives the content becoming ciphertext.
  const correlate = (item) => item.iv || item.content;

  let room = null;
  let source = null;
  let pendingSeq = 0;
  let countdownTimer = null;

  // --- lifecycle ---

  async function boot() {
    try {
      room = await resolveRoom();
    } catch (err) {
      setStatus('offline', 'No room');
      console.error('room bootstrap failed', err);
      return;
    }
    connect(await backfill());
  }

  // Three ways in, in priority order: the URL you opened (a scan, or a shared
  // link), the room this device already knows, or a brand new room.
  async function resolveRoom() {
    const fromPath = location.pathname.match(/^\/r\/([a-z0-9]+)\/?$/)?.[1];
    if (fromPath && (await roomExists(fromPath))) {
      localStorage.setItem(STORAGE_ROOM, fromPath);
      return fromPath;
    }

    const stored = localStorage.getItem(STORAGE_ROOM);
    if (stored && (await roomExists(stored))) {
      showRoomInURL(stored);
      return stored;
    }

    // Either nothing was stored, or the server restarted and lost it. Losing
    // everything on restart is correct for content that expires in 24h.
    const res = await fetch('/api/rooms', { method: 'POST' });
    if (!res.ok) throw new Error(`create room: ${res.status}`);
    const { id } = await res.json();
    localStorage.setItem(STORAGE_ROOM, id);
    showRoomInURL(id);
    return id;
  }

  async function roomExists(id) {
    try {
      const res = await fetch(`/api/rooms/${encodeURIComponent(id)}/items`);
      return res.ok;
    } catch {
      return false;
    }
  }

  // Keeps location.hash intact -- that is where the key lives from M3, and the
  // browser never sends it anywhere.
  function showRoomInURL(id) {
    if (location.pathname === `/r/${id}`) return;
    history.replaceState(null, '', `/r/${id}${location.hash}`);
  }

  // Renders what the room already holds and returns the newest ID seen, so the
  // stream can resume from there instead of replaying the same backlog.
  async function backfill() {
    const res = await fetch(`/api/rooms/${room}/items`);
    if (!res.ok) return '';
    const { items } = await res.json();
    for (const item of items) upsert(item, { announce: false });
    return items.length ? items[items.length - 1].id : '';
  }

  function connect(since) {
    setStatus('connecting', 'Connecting');
    // EventSource cannot set a Last-Event-ID header on the first connect, so
    // the resume point rides the query string. On every reconnect after that
    // the browser sends the real header and the server prefers it, leaving
    // this stale value ignored.
    const url = `/api/rooms/${room}/stream${since ? `?last_event_id=${encodeURIComponent(since)}` : ''}`;
    source = new EventSource(url);
    source.onopen = () => setStatus('live', 'Live');
    source.onerror = onStreamError;
    source.addEventListener('item', (e) => upsert(JSON.parse(e.data)));
    source.addEventListener('deleted', (e) => remove(JSON.parse(e.data).id));
  }

  // EventSource reconnects on its own and replays via Last-Event-ID, so the
  // usual case needs nothing from us but a status change.
  //
  // The case it cannot handle is the room being gone -- swept after 48h idle,
  // or lost to a restart. Then it would retry against a 404 forever, showing
  // "Offline" at someone whose network is fine. Check once, and rebuild.
  let checkingRoom = false;
  async function onStreamError() {
    setStatus('offline', 'Offline');
    if (checkingRoom) return;
    checkingRoom = true;
    try {
      if (await roomExists(room)) return; // ordinary blip; let it retry
      source.close();
      setStatus('offline', 'Room expired');
      localStorage.removeItem(STORAGE_ROOM);
      location.assign('/');
    } finally {
      checkingRoom = false;
    }
  }

  // Last-Event-ID covers a clean reconnect, but iOS sometimes resumes a tab
  // without re-establishing the stream properly, and the tab then sits there
  // looking connected with nothing arriving. This is the single most likely
  // "it didn't work" moment in the product, so it gets a belt as well as
  // braces: on every return to visibility, ask for anything newer than the
  // newest item we hold.
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') catchUp();
  });
  window.addEventListener('pageshow', catchUp);

  async function catchUp() {
    if (!room) return;
    try {
      const res = await fetch(`/api/rooms/${room}/items?since=${encodeURIComponent(newestID())}`);
      if (!res.ok) return;
      const { items } = await res.json();
      for (const item of items) upsert(item, { announce: false });
      if (items.length) {
        els.live.textContent = `${items.length} new item${items.length > 1 ? 's' : ''} while you were away`;
      }
    } catch (err) {
      console.error('catch-up failed', err);
    }
  }

  // Newest acknowledged item. Optimistic ones are skipped: their temp IDs are
  // not ULIDs and would poison the comparison.
  function newestID() {
    let newest = '';
    for (const [id, { el }] of rendered) {
      if (!el.dataset.pending && id > newest) newest = id;
    }
    return newest;
  }

  // --- sending ---

  // Document-level paste. Not a focused input: the paste event hands us
  // clipboardData with no permission prompt anywhere, including iOS.
  // navigator.clipboard.readText() would prompt, and is never used.
  document.addEventListener('paste', (e) => {
    // Let a paste into the pairing code field behave like a normal paste.
    // e.target is whatever had focus -- an element, <body>, or the document
    // itself, which has no closest(). Hence the optional call.
    if (e.target?.closest?.('input, textarea')) return;
    const text = e.clipboardData?.getData('text');
    if (!text || !room) return;
    e.preventDefault();
    send(text);
  });

  async function send(content) {
    // Optimistic echo: on screen now, reconciled later. The UI never waits on
    // the network.
    const tempId = `pending-${++pendingSeq}`;
    const now = new Date().toISOString();
    const pending = {
      id: tempId,
      content,
      origin: 'This device',
      created_at: now,
      expires_at: new Date(Date.now() + 86400000).toISOString(),
    };
    upsert(pending, { announce: false, pending: true });
    inflight.set(correlate(pending), tempId);

    try {
      const res = await fetch(`/api/rooms/${room}/items`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ content }),
      });
      if (res.status === 429) {
        const entry = rendered.get(tempId);
        if (entry) entry.el.dataset.failed = 'rate-limited';
        els.live.textContent = 'Sending too fast — that one did not go through';
        inflight.delete(correlate(pending));
        return;
      }
      if (res.status === 413) {
        const entry = rendered.get(tempId);
        if (entry) entry.el.dataset.failed = 'too-large';
        els.live.textContent = 'That was too big to send';
        inflight.delete(correlate(pending));
        return;
      }
      if (!res.ok) throw new Error(`send: ${res.status}`);
      const item = await res.json();
      // Whichever got here first -- this response or the stream echo -- one of
      // these is already a no-op.
      inflight.delete(correlate(item));
      remove(tempId);
      upsert(item, { announce: false });
    } catch (err) {
      console.error('send failed', err);
      inflight.delete(correlate(pending));
      const entry = rendered.get(tempId);
      if (entry) entry.el.dataset.failed = 'true';
    }
  }

  // --- rendering ---

  function upsert(item, { announce = true, pending = false } = {}) {
    if (rendered.has(item.id)) return; // duplicate delivery -- expected, ignore

    // Our own paste, coming back to us. Retire the optimistic copy and stay
    // quiet: the person who pasted it does not need it read out to them.
    if (!pending) {
      const tempId = inflight.get(correlate(item));
      if (tempId !== undefined) {
        inflight.delete(correlate(item));
        remove(tempId);
        announce = false;
      }
    }

    const el = els.template.content.firstElementChild.cloneNode(true);
    el.dataset.id = item.id;
    // ULIDs sort lexicographically by time. '~' sorts above every Crockford
    // base32 character, so optimistic items pin to the top until they resolve.
    el.dataset.sort = pending ? `~${item.id}` : item.id;
    if (pending) el.dataset.pending = 'true';

    // Each scrap sits very slightly askew, the same way every time. Derived
    // from the ID so it survives re-renders and matches across devices.
    el.style.setProperty('--rotation', `${seededRotation(item.id)}deg`);

    $('origin', el).textContent = item.origin;
    $('content', el).textContent = item.content;
    $('copy', el).setAttribute('aria-label', `Copy item from ${item.origin}`);
    $('delete', el).setAttribute('aria-label', `Throw away item from ${item.origin}`);

    if (isLong(item.content)) {
      el.dataset.long = 'true';
      $('expand', el).hidden = false;
    }
    applyAge(el, item);

    insertSorted(el);
    rendered.set(item.id, { item, el });
    syncChrome();

    if (announce) {
      // Metadata only. The live region never speaks the content aloud.
      els.live.textContent = `Item received from ${item.origin}`;
    }
  }

  // Sum of char codes, mapped to +/- 0.8 degrees. Cheap, stable, and enough
  // spread that no two adjacent scraps look aligned.
  function seededRotation(id) {
    let sum = 0;
    for (let i = 0; i < id.length; i++) sum += id.charCodeAt(i);
    return ((sum % 16) - 8) / 10;
  }

  const isLong = (text) => text.split('\n').length > 5 || text.length > 200;

  function insertSorted(el) {
    const key = el.dataset.sort;
    for (const sibling of els.list.children) {
      if (key > sibling.dataset.sort) {
        els.list.insertBefore(el, sibling); // newest first
        return;
      }
    }
    els.list.appendChild(el);
  }

  function remove(id) {
    const entry = rendered.get(id);
    if (!entry) return;
    const wasFocused = entry.el.contains(document.activeElement) || document.activeElement === entry.el;
    const next = entry.el.nextElementSibling || entry.el.previousElementSibling;
    entry.el.remove();
    rendered.delete(id);
    syncChrome();
    if (wasFocused && next) focusScrap(next);
  }

  // Fresh -> Fading -> Dying, driven by how much of the TTL is left. The design
  // reads data-age off the element; the label is here so it works unstyled.
  function applyAge(el, item) {
    const created = Date.parse(item.created_at);
    const expires = Date.parse(item.expires_at);
    const left = (expires - Date.now()) / (expires - created);
    const age = left > 0.5 ? 'fresh' : left > 0.1 ? 'fading' : 'dying';
    el.dataset.age = age;
    $('age', el).textContent = age[0].toUpperCase() + age.slice(1);
  }

  setInterval(() => {
    for (const { item, el } of rendered.values()) {
      if (!el.dataset.pending) applyAge(el, item);
    }
  }, 60000);

  function syncChrome() {
    const any = rendered.size > 0;
    els.empty.hidden = any;
    els.clear.hidden = !any;
  }

  // --- actions ---

  async function copyItem(id) {
    const entry = rendered.get(id);
    if (!entry) return;
    try {
      // Requires a secure context: localhost or HTTPS, no exceptions.
      await navigator.clipboard.writeText(entry.item.content);
      entry.el.dataset.copied = 'true';
      els.live.textContent = 'Copied to clipboard';
      setTimeout(() => delete entry.el.dataset.copied, 2000);
    } catch (err) {
      console.error('copy failed -- is this a secure context?', err);
      entry.el.dataset.copyFailed = 'true';
      els.live.textContent = 'Could not copy';
    }
  }

  async function deleteItem(id) {
    remove(id); // optimistic; the stream confirms for everyone else
    await fetch(`/api/rooms/${room}/items/${id}`, { method: 'DELETE' }).catch((err) =>
      console.error('delete failed', err)
    );
  }

  els.clear.addEventListener('click', async () => {
    for (const id of [...rendered.keys()]) remove(id);
    await fetch(`/api/rooms/${room}/items`, { method: 'DELETE' }).catch((err) =>
      console.error('clear failed', err)
    );
  });

  els.list.addEventListener('click', (e) => {
    const scrap = e.target.closest('[data-role="scrap"]');
    if (!scrap) return;
    if (e.target.closest('[data-role="copy"]')) copyItem(scrap.dataset.id);
    else if (e.target.closest('[data-role="delete"]')) deleteItem(scrap.dataset.id);
    else if (e.target.closest('[data-role="expand"]')) expand(scrap);
  });

  function expand(scrap) {
    scrap.dataset.expanded = 'true';
    $('expand', scrap).hidden = true;
  }

  // --- pairing ---

  els.pair.addEventListener('click', openPairing);
  els.pairClose.addEventListener('click', () => els.sheet.close());
  els.sheet.addEventListener('close', stopCountdown);

  async function openPairing() {
    els.joinError.textContent = '';
    els.pairCode.textContent = '—';
    els.pairQR.src = `/api/rooms/${room}/qr.png`;
    els.sheet.showModal();

    try {
      const res = await fetch('/api/pair', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        // M3 adds `payload`: the room key wrapped under a secret derived from
        // the code, so the typed path still works without the server ever
        // holding key material.
        body: JSON.stringify({ room }),
      });
      if (!res.ok) throw new Error(`mint: ${res.status}`);
      const { code, expires_in } = await res.json();
      els.pairCode.textContent = code;
      startCountdown(expires_in);
    } catch (err) {
      console.error('could not mint a pairing code', err);
      els.pairCode.textContent = 'unavailable';
    }
  }

  function startCountdown(seconds) {
    stopCountdown();
    let left = seconds;
    const tick = () => {
      if (left <= 0) {
        els.pairCountdown.textContent = 'now — reopen for a new code';
        stopCountdown();
        return;
      }
      const m = Math.floor(left / 60);
      const s = String(left % 60).padStart(2, '0');
      els.pairCountdown.textContent = m > 0 ? `${m}:${s}` : `${left}s`;
      left -= 1;
    };
    tick();
    countdownTimer = setInterval(tick, 1000);
  }

  function stopCountdown() {
    if (countdownTimer) clearInterval(countdownTimer);
    countdownTimer = null;
  }

  els.joinForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    const code = els.joinInput.value.trim();
    if (!code) return;
    els.joinError.textContent = '';

    try {
      const res = await fetch(`/api/pair/${encodeURIComponent(code)}`, { method: 'POST' });
      if (!res.ok) {
        const { error } = await res.json().catch(() => ({}));
        els.joinError.textContent = error || 'That code is not valid.';
        return;
      }
      const { room: joined } = await res.json();
      localStorage.setItem(STORAGE_ROOM, joined);
      // A real navigation rather than swapping state in place: it rebuilds the
      // stream, the list and the room from scratch, with nothing left over.
      location.assign(`/r/${joined}`);
    } catch (err) {
      console.error('join failed', err);
      els.joinError.textContent = 'Could not reach the server.';
    }
  });

  // --- keyboard ---

  // Roving tabindex: one scrap in the tab order at a time, arrows move between
  // them. Enter copies, Backspace throws away.
  function focusScrap(el) {
    for (const scrap of els.list.children) scrap.tabIndex = -1;
    el.tabIndex = 0;
    el.focus();
  }

  els.list.addEventListener('keydown', (e) => {
    const scrap = e.target.closest('[data-role="scrap"]');
    if (!scrap) return;

    switch (e.key) {
      case 'ArrowDown':
        if (scrap.nextElementSibling) {
          e.preventDefault();
          focusScrap(scrap.nextElementSibling);
        }
        break;
      case 'ArrowUp':
        if (scrap.previousElementSibling) {
          e.preventDefault();
          focusScrap(scrap.previousElementSibling);
        }
        break;
      case 'Enter':
        if (e.target === scrap) {
          e.preventDefault();
          copyItem(scrap.dataset.id);
        }
        break;
      case 'Backspace':
      case 'Delete':
        if (e.target === scrap) {
          e.preventDefault(); // Backspace must not navigate back
          deleteItem(scrap.dataset.id);
        }
        break;
    }
  });

  // Entering the list from the page tab order lands on the newest scrap.
  els.list.addEventListener('focusin', (e) => {
    const scrap = e.target.closest('[data-role="scrap"]');
    if (scrap && scrap.tabIndex !== 0) {
      for (const other of els.list.children) other.tabIndex = -1;
      scrap.tabIndex = 0;
    }
  });

  const observer = new MutationObserver(() => {
    const first = els.list.firstElementChild;
    if (first && ![...els.list.children].some((c) => c.tabIndex === 0)) first.tabIndex = 0;
  });
  observer.observe(els.list, { childList: true });

  // --- status ---

  function setStatus(state, label) {
    els.status.dataset.state = state;
    els.statusLabel.textContent = label;
  }

  boot();
})();
