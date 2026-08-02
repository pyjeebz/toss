// Toss -- client.
//
// Paste to send, EventSource to receive, tap to copy back. Rooms live at
// /r/<room> and are remembered in localStorage; a short code or a QR scan
// brings another device into the same room.
//
// Everything on the wire is AES-GCM ciphertext (M3). The key is generated here,
// lives in the URL fragment, and is never sent anywhere -- see crypto.js. The
// consequence for this file is that plaintext exists in exactly two places: the
// `text` field of a `rendered` entry, and the local variable it was decrypted
// into. It is never stored, never logged, and never put in an attribute.

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
    compose: $('compose'),
    composeForm: $('compose-form'),
    composeSend: $('compose-send'),
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

  // localStorage holds the room ID and the key, as one entry so they cannot
  // drift apart. Never item content.
  const STORAGE = 'toss.room';

  // id -> { item, el, text }. The single source of truth for what is on screen,
  // and what makes duplicate delivery (stream replay overlapping a POST
  // response) a non-event. `text` is the decrypted content, held only here, and
  // null when this device could not read the item.
  const rendered = new Map();

  // Correlation key -> temp id, for sends that have not come back yet. Lets an
  // item arriving on the stream be recognised as the echo of our own optimistic
  // render rather than a new arrival, whichever of the two lands first.
  const inflight = new Map();

  // The IV: generated here, unique per item by construction, and unchanged as
  // the content travels as ciphertext. The content itself cannot serve -- two
  // encryptions of the same text differ.
  const correlate = (item) => item.iv;

  let room = null;
  let key = null; // CryptoKey; never leaves this scope
  let keyB64 = null; // the same key, for the fragment and the QR
  let source = null;
  let pendingSeq = 0;
  let countdownTimer = null;

  // --- lifecycle ---

  async function boot() {
    // WebCrypto is exposed only in a secure context, so on plain HTTP to
    // anything but localhost, crypto.subtle is simply undefined. Before M3 that
    // cost you tap-to-copy; now it costs you the whole app, because there is no
    // unencrypted mode to fall back to. Say so in words rather than failing
    // with a null dereference in the console.
    if (!window.isSecureContext || !window.crypto?.subtle) {
      setStatus('offline', 'Needs HTTPS');
      els.pair.disabled = true;
      els.compose.disabled = true;
      els.empty.hidden = false;
      els.empty.textContent =
        'Toss encrypts everything in the browser, and browsers only allow that ' +
        'over a secure connection. Open this page over https:// — or on ' +
        'localhost — and it will work.';
      console.error('WebCrypto needs a secure context. Use HTTPS, or localhost.');
      return;
    }

    try {
      await resolveSession();
    } catch (err) {
      setStatus('offline', 'No room');
      console.error('room bootstrap failed', err);
      return;
    }
    connect(await backfill());
  }

  // Three ways in, in priority order: the URL you opened (a scan, or a link
  // someone sent you), the room this device already knows, or a brand new one.
  //
  // A room is only usable with its key, so both halves have to arrive together.
  // A room ID with no key is not a session -- it is a room whose contents this
  // device cannot read -- so those cases fall through to minting a fresh one.
  async function resolveSession() {
    const fromPath = location.pathname.match(/^\/r\/([a-z0-9]+)\/?$/)?.[1];
    const fromHash = new URLSearchParams(location.hash.slice(1)).get('k');
    if (fromPath && fromHash && (await roomExists(fromPath))) {
      if (await adopt(fromPath, fromHash)) return;
    }

    const stored = readStored();
    if (stored && (await roomExists(stored.id))) {
      if (await adopt(stored.id, stored.k)) return;
    }

    // Either nothing was stored, or the server restarted and lost it. Losing
    // everything on restart is correct for content that expires in 24h.
    const id = await createRoom();
    if (!(await adopt(id, await TossCrypto.exportKey(await TossCrypto.generateKey())))) {
      throw new Error('could not adopt a freshly minted room');
    }
  }

  // Takes a room and its key. Returns false rather than throwing if the key is
  // unusable -- a truncated fragment or a mangled localStorage entry should
  // send us on to the next option, not kill the boot.
  async function adopt(id, k) {
    try {
      key = await TossCrypto.importKey(k);
    } catch (err) {
      console.error('that key is not usable', err);
      return false;
    }
    room = id;
    keyB64 = k;
    localStorage.setItem(STORAGE, JSON.stringify({ id, k }));
    showRoomInURL();
    return true;
  }

  function readStored() {
    try {
      const { id, k } = JSON.parse(localStorage.getItem(STORAGE)) || {};
      return id && k ? { id, k } : null;
    } catch {
      return null; // pre-M3 entry, or something else wrote here
    }
  }

  // The restart stampede: when the server restarts, every open tab discovers
  // its room is gone at the same instant and tries to mint. Behind one NAT only
  // ten get through in the hour, and the rest used to sit on "No room" until
  // someone reloaded them.
  //
  // Retrying turns that into a tab that heals itself. The jitter matters more
  // than the curve: what has to be broken up is the synchronisation, since the
  // whole problem is that every tab woke at the same moment.
  async function createRoom() {
    for (let attempt = 0; ; attempt++) {
      const res = await fetch('/api/rooms', { method: 'POST' });
      if (res.ok) return (await res.json()).id;
      if (res.status !== 429) throw new Error(`create room: ${res.status}`);
      setStatus('offline', 'Waiting for a room');
      await sleep(Math.random() * Math.min(60000, 1000 * 2 ** attempt));
    }
  }

  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  async function roomExists(id) {
    try {
      const res = await fetch(`/api/rooms/${encodeURIComponent(id)}/items`);
      return res.ok;
    } catch {
      return false;
    }
  }

  // The fragment is the key's home: it survives reload, it is what the QR
  // encodes, and the browser never puts it on the wire.
  function showRoomInURL() {
    const want = `/r/${room}#k=${keyB64}`;
    if (location.pathname + location.hash === want) return;
    history.replaceState(null, '', want);
  }

  // Renders what the room already holds and returns the newest ID seen, so the
  // stream can resume from there instead of replaying the same backlog.
  async function backfill() {
    const res = await fetch(`/api/rooms/${room}/items`);
    if (!res.ok) return '';
    const { items } = await res.json();
    // Decryption order does not matter: insertSorted places by ULID, so the
    // list is right however these land.
    await Promise.all(items.map((item) => accept(item, { announce: false })));
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
    source.addEventListener('item', (e) => accept(JSON.parse(e.data)));
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
      // The key goes with the room. Navigating to '/' drops the fragment too,
      // so the next boot mints both halves fresh.
      localStorage.removeItem(STORAGE);
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
      await Promise.all(items.map((item) => accept(item, { announce: false })));
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

  // Document-level paste. The paste event hands us clipboardData with no
  // permission prompt anywhere, including iOS.
  // navigator.clipboard.readText() would prompt, and is never used.
  //
  // This covers desktop, where paste lands on the document. It cannot cover a
  // phone: touch browsers only offer "Paste" when an editable element has
  // focus, which is what the compose field is for.
  document.addEventListener('paste', (e) => {
    // e.target is whatever had focus -- an element, <body>, or the document
    // itself, which has no closest(). Hence the optional call.
    const field = e.target?.closest?.('input, textarea');
    // The pairing code field is the one place a paste should behave like an
    // ordinary paste. The compose field is the opposite: pasting there is how
    // you send from a phone, so it is intercepted rather than allowed to land.
    if (field && field.dataset.role !== 'compose') return;

    const text = e.clipboardData?.getData('text');
    if (!text || !room) return;
    e.preventDefault(); // so it never reaches the field, which stays empty
    resetCompose();
    send(text);
  });

  // Typed text is not sent until it is finished with -- a paste is already a
  // deliberate act, whereas typing is in progress until you say otherwise.
  els.composeForm.addEventListener('submit', (e) => {
    e.preventDefault();
    submitCompose();
  });

  els.compose.addEventListener('keydown', (e) => {
    // Enter sends, Shift+Enter makes a new line. The usual convention, and it
    // keeps the field usable for the occasional multi-line note.
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      submitCompose();
    }
  });

  els.compose.addEventListener('input', syncCompose);

  function submitCompose() {
    const text = els.compose.value;
    if (!text.trim() || !room) return;
    resetCompose();
    send(text);
  }

  function syncCompose() {
    // Send appears only once there is something to send, so the desktop layout
    // stays as drawn until it is needed.
    els.composeSend.hidden = els.compose.value.trim() === '';
    // Grow with the content instead of scrolling a one-line box. Reset first,
    // or scrollHeight only ever reports the height it already has.
    els.compose.style.height = 'auto';
    els.compose.style.height = `${els.compose.scrollHeight}px`;
  }

  function resetCompose() {
    els.compose.value = '';
    syncCompose();
  }

  async function send(text) {
    // Encrypt first: the IV is the handle that ties the optimistic render to
    // the server's echo, and it has to exist before either happens.
    let sealed;
    try {
      sealed = await TossCrypto.encrypt(key, text);
    } catch (err) {
      console.error('encrypt failed', err);
      els.live.textContent = 'Could not encrypt that — nothing was sent';
      return;
    }

    // Optimistic echo: on screen now, reconciled later. The UI never waits on
    // the network.
    const tempId = `pending-${++pendingSeq}`;
    const now = new Date().toISOString();
    const pending = {
      id: tempId,
      iv: sealed.iv,
      content: sealed.content,
      origin: 'This device',
      created_at: now,
      expires_at: new Date(Date.now() + 86400000).toISOString(),
    };
    // We already hold the plaintext, so this render skips the round trip
    // through decrypt entirely.
    upsert(pending, text, { announce: false, pending: true });
    inflight.set(correlate(pending), tempId);

    try {
      const res = await fetch(`/api/rooms/${room}/items`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ iv: sealed.iv, content: sealed.content }),
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
      upsert(item, text, { announce: false });
    } catch (err) {
      console.error('send failed', err);
      inflight.delete(correlate(pending));
      const entry = rendered.get(tempId);
      if (entry) entry.el.dataset.failed = 'true';
    }
  }

  // --- rendering ---

  // Everything arriving from the network comes through here: decrypt, then
  // render. Rendering stays synchronous on purpose, so the duplicate check at
  // the top of upsert() is still atomic with respect to the insert -- two
  // deliveries of the same item can both get past the check here, and only one
  // can get past the one in there.
  async function accept(item, opts) {
    if (rendered.has(item.id)) return;
    let text = null;
    try {
      text = await TossCrypto.decrypt(key, item.iv, item.content);
    } catch {
      // Wrong key, no key, or a byte moved in transit. AES-GCM cannot tell
      // those apart and neither can we. Render it anyway: a scrap that says it
      // cannot be read is honest, whereas a silent gap looks like the product
      // quietly losing someone's paste.
    }
    upsert(item, text, opts);
  }

  function upsert(item, text, { announce = true, pending = false } = {}) {
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
    $('copy', el).setAttribute('aria-label', `Copy item from ${item.origin}`);
    $('delete', el).setAttribute('aria-label', `Throw away item from ${item.origin}`);

    if (text === null) {
      el.dataset.undecryptable = 'true';
      $('content', el).textContent = 'Sent with a different key — this device cannot read it.';
    } else {
      $('content', el).textContent = text;
      if (isLong(text)) {
        el.dataset.long = 'true';
        $('expand', el).hidden = false;
      }
    }
    applyAge(el, item);

    insertSorted(el);
    rendered.set(item.id, { item, el, text });
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
    if (entry.text === null) {
      els.live.textContent = 'Cannot copy — this device does not have the key';
      return;
    }
    try {
      // Requires a secure context: localhost or HTTPS, no exceptions.
      await navigator.clipboard.writeText(entry.text);
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
    drawPairQR();
    els.sheet.showModal();

    try {
      const { code, expires_in } = await mintPairCode();
      els.pairCode.textContent = code;
      startCountdown(expires_in);
    } catch (err) {
      console.error('could not mint a pairing code', err);
      els.pairCode.textContent = 'unavailable';
    }
  }

  // The code is chosen here, not by the server, and that is a requirement
  // rather than a preference: `payload` is the room key sealed under a secret
  // derived from the code, and the server is holding the payload. If it also
  // chose the code it would hold both halves and could read the room. Anything
  // the server picks, the server knows.
  //
  // A collision is a 1-in-2^40 event, so it is handled by picking again.
  async function mintPairCode() {
    for (let attempt = 0; attempt < 3; attempt++) {
      const code = TossCrypto.newPairCode();
      const payload = await TossCrypto.wrapForCode(code, key);
      const res = await fetch('/api/pair', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ room, code, payload }),
      });
      if (res.ok) return res.json();
      if (res.status !== 409) throw new Error(`mint: ${res.status}`);
    }
    throw new Error('could not find a free pairing code');
  }

  // Drawn here rather than fetched from the server, and that is the whole
  // point: the code has to contain location.hash, which is where the room key
  // lives from M3 and is precisely what the browser never sends anywhere. A
  // server-rendered QR could only encode what the server knows, so a scan
  // could not carry the key. See qr.js.
  //
  // location.origin also beats anything the server could infer about its own
  // public URL -- it is, by definition, the origin this page was actually
  // loaded from, proxies and all.
  function drawPairQR() {
    try {
      els.pairQR.src = TossQR.toDataURL(`${location.origin}/r/${room}${location.hash}`);
      els.pairQR.hidden = false;
    } catch (err) {
      // Only reachable if the URL outgrows a version 20 code. The typed
      // pairing code below is a complete path on its own, so lose the image
      // rather than the sheet.
      console.error('could not draw the QR', err);
      els.pairQR.removeAttribute('src');
      els.pairQR.hidden = true;
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
      const { room: joined, payload } = await res.json();

      // The code did two jobs: the server used it to find the room, and it
      // unwraps the key here. Only the second one is a secret the server never
      // learns, so this is where joining actually succeeds or fails.
      let k;
      try {
        k = await TossCrypto.exportKey(await TossCrypto.unwrapWithCode(code, payload));
      } catch (err) {
        console.error('the code did not unwrap the room key', err);
        els.joinError.textContent = 'That code did not unlock the room. Ask for a new one.';
        return;
      }

      localStorage.setItem(STORAGE, JSON.stringify({ id: joined, k }));
      // A real navigation rather than swapping state in place: it rebuilds the
      // stream, the list and the room from scratch, with nothing left over. The
      // key goes in the fragment, which is the one part of this URL that stays
      // on the device.
      location.assign(`/r/${joined}#k=${k}`);
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
