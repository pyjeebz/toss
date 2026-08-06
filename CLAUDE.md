# Toss

Move text between your own devices. Paste on one, it appears on the others.
No accounts, no install, no native app.

**Status: feature complete. M1–M4 all in.** Transport, rooms, pairing, expiry,
limits, recovery, the design and end-to-end encryption are done. What is left
before it is live is deployment, not code — see "Before it goes live".

The QR is generated in the browser (`web/qr.js`) and the content is encrypted
in the browser (`web/crypto.js`). The server has never been able to read either.

## Run it

```sh
go run ./cmd/toss              # :8080, or set TOSS_ADDR
go test -race ./...            # the hub is concurrent; always use -race
```

Open `/`. It mints a room and a key, rewrites to `/r/<room>#k=<key>` and
remembers both in `localStorage`. "Add device" shows a QR and an 8-character
code; another device scans, or types the code into the same sheet, and lands in
the same room with the same key.

**HTTPS is not optional, and since M3 it is not survivable either.** WebCrypto
is exposed only in a secure context, so on plain HTTP to anything but
`localhost`, `crypto.subtle` is `undefined` and there is nothing to fall back
to — there is no unencrypted mode. `boot()` checks for this and says so in
words; without the check it fails as a null dereference and a bare "No room".

`localhost` is a secure context, so dev over plain HTTP is fine. **Testing from
a phone on the LAN is not** — `http://192.168.x.x:8080` is not secure, and the
app will refuse to start. Use a tunnel with real TLS (`cloudflared tunnel --url
http://localhost:8080`, or ngrok) rather than the LAN IP.

The `qr.js` and `crypto.js` tests shell out to `node`. They skip if there isn't
one, so `go test ./...` still passes without it — but then nothing is checking
either file, which between them are the QR and the whole of the encryption.

## The one thing that will silently break this

**Exactly one process.** This has already bitten once in production: `fly
deploy` creates **two** machines by default, `min_machines_running = 1` is a
floor rather than a ceiling and does not prevent it, and the result was pastes
that silently never crossed between devices. Deploy with `--ha=false`.

To check from outside, hit `/healthz` a few times. A single process's room
count can only go up, so if it alternates there is more than one machine:

```sh
for i in $(seq 6); do curl -s https://<app>/healthz; echo; done
```

All state is in memory and there is no cross-process
fan-out. Two instances behind a load balancer means a POST landing on A while
the recipient's stream is held by B goes nowhere — no error, no log, a 201 to
the sender. Don't add replicas, don't autoscale. Scaling out is a shared bus
(Redis pub/sub, NATS) or sticky routing on room ID, and it is a real change.

## Decisions that are settled — don't revisit

- **SSE, not WebSockets.** `EventSource` gives reconnect and `Last-Event-ID`
  replay for free, and iOS Safari suspends background tabs constantly, so that
  path runs many times a day. Sends are infrequent and want a status code.
- **No database.** A restart losing everything is correct for content that
  expires in 24h.
- **Room ID ≠ pairing code.** Room ID is 120 bits of `crypto/rand`,
  unguessable. The pairing code (M1) is 8 typeable chars with a 5-minute TTL
  that *resolves* to a room ID. Short codes are brute-forceable; room IDs must
  not be.
- **Item content is opaque.** The server never parses, inspects, transforms or
  logs it. It is AES-GCM ciphertext and the key lives in the URL fragment, which
  browsers never send. This held through M3 with no change to `internal/hub` at
  all, and it only keeps holding **as long as nothing there ever reads
  content.** Don't write that code.
- **The client chooses the pairing code.** Not a style preference — a
  requirement. The `payload` on a pairing is the room key sealed under a secret
  derived from the code, and the server stores that payload. If the server also
  picked the code it would hold both halves and could open the room. Anything
  the server picks, the server knows. `POST /api/pair` therefore refuses a
  `payload` that arrives without a client-supplied `code`; see the note below.

## Settled: the QR is drawn in the browser

This used to be the one open question blocking M3, and it went with **option 1**
— the QR is encoded client-side by `web/qr.js`, and the server no longer renders
one at all. The scan is a complete handshake: `drawPairQR()` encodes
`location.origin + /r/<room> + location.hash`, and the fragment holds the key,
so the code carries it and the server never sees it.

`GET /api/rooms/{room}/qr.png` is **gone**. Do not bring it back, and do not add
an endpoint that renders a QR from a client-supplied URL — either one hands the
server the key and undoes the reason the key is in the fragment.

`TOSS_ORIGIN` went with it. The browser uses `location.origin`, which is by
definition the origin the page was actually loaded from, so there is nothing
left to misconfigure.

Option 2 (the key riding the typed code via the opaque `payload` on
`POST /api/pair`) is also built, as the fallback for someone typing the code
instead of scanning. It did need one server change, and the reason is worth
keeping: see the next section.

## Settled: the pairing code is generated in the browser

The original plan said the typed-code path needed no server change. **That was
wrong**, and the reason is a genuine hole rather than an inconvenience.

The payload is the room key sealed under a secret derived from the pairing code.
The server stores the payload. So if the server *also* generates the code, it
holds the key and the thing the key opens, and E2EE for the typed path is
decorative. Sealing something under a secret you were handed by the party you
are hiding it from is not encryption.

So `POST /api/pair` now takes an optional `code`, and:

- a `payload` **without** a `code` is a `400`. The combination that looks fine
  and is not is refused outright rather than documented against.
- the code must arrive canonical — exactly 8 characters, all in `pairAlphabet`.
  Normalisation is for what a person types back, not for what a client submits.
- a taken code is a `409` and the client picks another. At 40 bits this
  effectively never happens.
- no code and no payload still works, and the server still picks. That is the
  pre-M3 shape and there is no key material at stake in it.

The code is still 40 bits from a CSPRNG; all that changed is who draws it. A
client that draws badly weakens its own room and nobody else's.

### The normalisation is a two-language contract

`normalizePairCode` in `internal/api/pair.go` and `normalizeCode` in
`web/crypto.js` **must** agree. The server uses its copy to find the room; the
browser uses its copy to derive the key that unwraps it. Diverge by one
character and the failure is invisible from the server's side — the code
redeems, the right room comes back, and then nothing decrypts.

`internal/api/paircode_parity_test.go` pins them together over every ASCII code
point. The JS side uppercases **ASCII only**, deliberately: Go's
`strings.ToUpper` is Unicode-aware and the two disagree in both directions
(`ß` → `SS` in JS, unchanged in Go; `ſ` → `S` in Go, unchanged in JS). Neither
is reachable from a keyboard typing an 8-character code, and the restriction is
what makes the parity exact rather than approximate.

### qr.js is hand-written, so it is tested against a second implementation

Byte mode, EC level M, versions 1–20. A wrong table entry produces a code that
still *looks* like a QR and simply does not scan — a bug that reaches a phone
camera before it reaches a person. So `web/qr_test.go` runs `qr.js` under `node`
and compares against `go-qrcode`, module for module, at every version and on
both sides of every version boundary.

**`github.com/skip2/go-qrcode` is still in `go.mod`, as a test-only dependency.**
It is not linked into the binary. It stays because it is the reference the
hand-written encoder is checked against, and deleting it would leave a few
hundred lines of bit-twiddling with nothing to check it.

Two things about that test are easy to get wrong when editing it:

- **The mask is pinned.** Mask choice is a quality heuristic — any mask decodes
  — and the two encoders disagree on about a third of inputs. The disagreement
  is `go-qrcode`'s: its rule 1 scores a run of exactly five modules as zero
  where the spec scores it 3. A second test covers the un-pinned path the app
  actually ships, by checking the format information truthfully declares the
  mask applied and that undoing it leaves the reference's data.
- **The inputs are byte-mode only.** `go-qrcode` splits its input into numeric
  and alphanumeric segments where that is shorter; `qr.js` never does. The
  generated inputs avoid the alphanumeric charset (`0-9 A-Z $%*+-./:` and
  space) entirely so there is no segmentation to find and the comparison stays
  like for like. Feed it digits and it will report failures that are not bugs.

## crypto.js is the whole of M3, so it is tested for properties, not output

There is no second implementation to diff against here — AES-GCM is AES-GCM.
What `web/crypto_test.go` checks instead is the set of properties whose failure
is silent: the round trip preserves the text exactly (emoji, combining marks,
control bytes, 20 KB of it), the IV is 96 bits, the ciphertext is plaintext + a
16-byte tag, the plaintext is not sitting in the ciphertext, a wrong key is
refused, a single flipped bit is refused, and neither IVs nor ciphertexts ever
repeat across 300 encryptions of identical text.

`web/e2e_test.go` then runs the whole thing against a real `httptest` server:
device A mints a room, encrypts and sends; device B redeems a *typed* code,
unwraps, and reads the text back — while the test asserts that neither the
plaintext nor the key appears anywhere in what the server serves.

Both drive the real `crypto.js` under node, and every pass/fail decision is made
in Go. The driver does mechanism only, so a bug in the driver cannot vote itself
correct.

Two things to know before editing those tests:

- **`cryptoOp.At` and `.N` must not get `omitempty`.** `At: 0` is a meaningful
  offset, and omitting it leaves `op.at` undefined in the driver, where
  `bytes[undefined % len] ^= 1` tampers with nothing and the test passes for the
  wrong reason. This already happened once.
- **The "plaintext is not in the ciphertext" check decodes the base64 first**
  and skips inputs under 4 bytes. Substring-searching the base64 for a
  one-character plaintext matches by chance several percent of the time.

## Things that are load-bearing and look optional

- `WriteTimeout: 0` in `main.go`. Any deadline cuts SSE streams mid-flight.
- `BaseContext` in `main.go`. Cancelling it is how open streams are told to
  wind down; without it `Shutdown` blocks on them until its own deadline.
- `select` on `r.Context().Done()` in `sse.go`. Without it every disconnected
  client leaks a goroutine and a subscriber entry.
- Explicit `Flush()` after every event. Otherwise the response buffers and
  nothing arrives until the connection closes.
- Non-blocking send in `hub.broadcast`. One stalled phone on bad wifi must not
  stall the publisher for the whole room. Dropping is safe (clients recover via
  backfill); blocking is not.
- Subscribe *before* replaying backlog in `sse.go`. The other order drops
  anything published in between. The overlap can deliver twice — clients dedupe
  by ID.
- `deleted` events carry no `id:` field. Advancing `Last-Event-ID` to a deleted
  item's ULID would make a reconnect skip everything published since.
- `paired` events carry no `id:` field either, for the same reason, and are
  never replayed. It is a live notification; telling a reconnecting client
  about a pairing from an hour ago would be a lie told with a stamp on it.
- `presence` events carry no `id:` either, and are never replayed. Same
  reasoning again: a count from an hour ago is worse than no count.
- **The presence count is taken and broadcast under the same write lock**, via
  `broadcastLocked`. It is the only broadcast in `hub.go` that does not happen
  after the unlock, and the exception is load-bearing: take the count and send
  it in two steps and two devices arriving at once can interleave — take 5,
  take 6, send 6, send 5 — leaving a subscriber that already saw 6 sitting on
  5 forever, because presence is never replayed and nothing later corrects it.
  Sending under the lock is safe only because every send is non-blocking.
  `TestPresenceCountsNeverGoBackwardsWhileDevicesArrive` asserts monotonicity
  rather than the final value: checking only where the count ends up **passes
  against the broken version**, because the last write usually does carry the
  highest number. That was checked by mutation, not assumed.
- **`Subscribe` emits a presence event, so it is the first thing a new
  subscriber receives.** Tests that read `sub.C()` expecting an item must go
  through the `nextContent` helper in `hub_test.go`, or they get a count where
  they expected content.
- The QR carries the pairing code (`&c=`) as well as the key. Without it a scan
  is **invisible to the server** — it is just a room URL — so the device
  holding the QR would never learn the scan worked. Being in the fragment, the
  code still only reaches the server if the scanning device redeems it.
- `drawPairQR()` is called **twice** when the sheet opens: once immediately so
  there is something on screen, and again once the mint returns and there is a
  code to embed. Drop the second call and scans stop announcing themselves.
- Pairing codes are single-redemption and expire in 5 minutes. 40 bits is not
  much, so the window and the one-guess-per-code rule are what make it safe.
  Redeem failures return one message for "unknown", "used" and "expired" alike;
  telling them apart tells an attacker which guesses were close.
- `GET /r/{room}` does **not** check the room exists. At M3 the key is in the
  fragment, which never reaches the server, so the client has to be running
  before it can tell. A stale room is the client's problem, and it recovers by
  minting a new one.
- `qr.js` loads **before** `app.js` in `index.html`. Both are `defer`, which
  runs them in document order; swap the tags and `TossQR` is undefined at the
  moment the pairing sheet opens.
- The format information is written **twice**, and the second copy splits 8/7,
  not 7/8. Taking eight bits up the column would land on `(size-8, 8)` — the
  dark module, which must stay set. Both copies are also easy to write
  transposed, which produces a code that looks perfect and scans on nothing;
  that is what `qr_test.go` caught the first time round.
- The document-level `paste` handler guards with `e.target?.closest?.(...)`.
  `e.target` can be the document, which has no `closest`, and an exception there
  silently kills every send.
- **Reads are never rate limited.** Backfill on wake and stream reconnects are
  exactly what a phone does all day. Metering those breaks the product for the
  people using it correctly while barely inconveniencing anyone abusing it.
- Proxy headers are trusted only when `TOSS_TRUST_PROXY` is set. `X-Forwarded-For`
  is client-supplied; trusting it by default turns every per-IP limit into a
  suggestion, since one header per request buys a fresh budget. When it is set,
  the **last** entry wins — proxies append, so that is the hop we trust.
- `idle()` requires a room to have zero subscribers before it can be collected.
  A live stream does not refresh `lastSeen`, so without that check a tab left
  open for two days would have its room swept out from under it.
- `Since()` filters expired items as well as the sweeper collecting them. The
  sweeper runs once a minute and nothing should be served in the gap.
- `[hidden] { display: none !important }` in `app.css`. The UA rule is
  zero-specificity, so *any* author rule setting `display` unhides the element.
  `.action { display: inline-flex }` on mobile did exactly that, and "Show all"
  appeared on every scrap regardless of length.
- The `#rough-edge` filter is applied to the paper **layer**, never to an
  element containing text. `feDisplacementMap` on live glyphs makes them wobble.
  The Figma file applies it to the whole pairing panel; that is deliberately not
  copied, because the pairing code has to be read aloud and typed by hand.

## The compose field is not decorative

`.compose` in the dropzone is what makes the product two-way. Without it there
is **no way to send from a phone at all**, and that was true for longer than it
should have been: the document-level `paste` listener covers desktop, but a
touch browser will not offer "Paste" unless an editable element has focus, and
until this went in the only field on the page was the pairing code input, which
the paste handler explicitly ignores. Laptop → phone worked; phone → laptop did
not exist.

How it behaves, and why:

- **A paste into it sends immediately and never lands in the field.** The
  handler calls `preventDefault()`, so the field stays empty and there is
  nothing to clear. This matches desktop, where pasting anywhere sends.
- **Typed text waits** for Send or Enter (Shift+Enter for a newline). A paste is
  already a deliberate act; typing is in progress until you say otherwise.
- The document-level handler skips `input, textarea` **except** the compose
  field, which it identifies by `data-role`. Do not go back to skipping every
  field — that is precisely the bug.
- **`font-size: 1rem` on `.compose-field` is a floor, not a preference.** iOS
  Safari zooms the whole page when a field below 16px takes focus, and does not
  zoom back out.

`navigator.clipboard.readText()` was considered and rejected for this. It would
save one tap on Android and cost a system permission dialog on every send on
iOS, does not work on Firefox, and would need the field as a fallback anyway —
so it buys a second implementation and a prompt.

## Settled: the restart stampede, handled in the client

Recovery-from-a-missing-room (M2) and the 10 rooms/hr per-IP cap interact
badly. When the server restarts, **every open tab** discovers its room is gone
and mints a new one. Behind a single NAT, only 10 succeed in the hour; the rest
sat on "No room" until the budget refilled. This showed up for real while
testing — abandoned browser tabs exhausted the budget and starved later runs.

Fixed with the third of the three options that were on the table: **the client
backs off and retries** rather than minting once and giving up. `createRoom()`
in `app.js` loops on a 429 with exponential backoff and full jitter, capped at a
minute, showing "Waiting for a room" while it waits.

The other two options were rejected because they are deployment guesses. A
higher cap is a number nobody can pick correctly in advance, and a bucket
exemption for recovery is an exemption an attacker can also claim. Backoff needs
to know nothing about the deployment and makes the tab heal itself.

The jitter is the part that matters, and it is not the same thing as the
backoff. The whole problem is that every tab woke at the same instant, so what
has to be broken up is the synchronisation, not the rate. `Math.random() *
min(60s, 2^n)` — full jitter, not a fixed multiplier.

Note this does **not** raise the cap: a genuine flood still gets 429s. It only
changes what a legitimate tab does when it loses.

## Settled: the manifest starts at `/`, and cannot start anywhere else

`web/manifest.webmanifest` makes the app installable to a home screen. It is
optional in every sense — nothing about the app changes if nobody installs it,
and there is no service worker, so nothing is cached and no install banner is
pushed at anyone.

**`start_url` is `/` and must stay `/`.** The instinct is to point it at the
room so launching opens straight into your scraps. Both ways of doing that
break E2EE:

- **A per-room manifest** has to be generated by the server, which means the
  server writes a file containing `#k=<key>`. That is the exact thing the URL
  fragment exists to prevent — same category as the `qr.png` endpoint that got
  deleted.
- **A client-built `data:` manifest** keeps the key in the browser, but the CSP
  refuses it: `manifest-src` falls back to `default-src 'self'`. Loosening the
  CSP for a launcher shortcut is a bad trade.

Neither is necessary, because `resolveSession()` in `app.js` already reads the
room and key back out of `localStorage` when it lands on `/`. The launcher
never needs them in the URL at all.

JSON cannot carry a comment, so
`web/manifest_test.go:TestManifestStartsAtTheRootAndCarriesNoRoomOrKey` carries
it instead — it pins `start_url`, and separately greps the raw file for `/r/`,
`#k` and `k=` so a room smuggled into some other field still fails.

Other things worth knowing before editing this:

- **`display` is `standalone`, and that is the more private option.** Hiding
  the URL bar means the key is not on screen to be caught by a screenshot or a
  screen share. `minimal-ui` would keep a visible reminder that the URL *is*
  the secret, which is the argument the other way, but a key nobody can
  photograph beats a reminder.
- **No service worker, deliberately.** It would let Android show an automatic
  install prompt rather than a menu item, and that is not worth it: a cached
  `app.js` or `crypto.js` can leave a device running an old client against a
  changed pairing contract, decrypting nothing while reporting success. It is
  also how a project drifts into the offline queue that Scope rules out.
- **The MIME type is registered in `internal/api/routes.go`'s `init()`.** Go's
  table has no `.webmanifest` entry, `http.ServeContent` then sniffs and answers
  `text/plain`, and Chrome silently ignores a manifest served that way.
- **`web/icons/generate.sh` is not embedded.** `embed.go` lists the icon files
  individually rather than embedding the directory, so the build tool stays out
  of the binary. Add an icon, add it to that list.
- **The maskable icon is a separate file with no corner radius.** Android crops
  to its own shape; cropping artwork that is already rounded eats the corners.
  `icon.svg` is the only source — `generate.sh` derives the full-bleed variant
  from it with `sed`.

**Checked on a real iPhone (2026-08-05): iOS shares `localStorage` between
Safari and the home-screen app.** Pair in Safari, add to the home screen, open
it from the icon, and you land in the same room — `resolveSession()` finds the
stored `{id, k}` and adopts it, exactly as it does for a returning tab.

This was the one open question, because iOS has historically given installed web
apps their own storage jar. Had it still been partitioned, installing after
pairing would have fallen through to `createRoom()` and handed the user a new
empty room, and "installing counts as a new device" would have had to be said
in the pairing sheet rather than discovered. It does not, so there is nothing to
say and nothing to build.

Worth re-checking if iOS ever changes it: the symptom is that installing looks
like it silently forgot your scraps.

## Fonts

Self-hosted in `web/fonts/`, embedded in the binary. The CSP forbids external
hosts, and a font request to Google is a third party learning who reads a room.

| | |
|---|---|
| **Instrument Sans** | **Bundled.** OFL, see `fonts/OFL-InstrumentSans.txt`. One variable file covers 400–700. |
| **Redaction 35** | **Bundled.** Dual licensed OFL 1.1 / LGPL 2.1, see `fonts/OFL-Redaction.txt`. Regular only — `--font-display` is only ever set at 400. |
| **Commit Mono** | **Not bundled.** Licence unverified from here. |

To add Commit Mono: drop the woff2 into `web/fonts/` and uncomment its
`@font-face` at the top of `app.css`. Until then `--font-mono` falls through to
a sensible stack and the layout is unaffected.

**Redaction came from `redaction.us`, not the CDN the Figma file points at.**
That mirror (`fonts.cdnfonts.com/css/redaction`) is what made this unbundled
originally — it ships no licence file. The official package ships webfonts, an
`OFL.txt`, and the OFL declared inside the font binary's own name table, which
is what verified it. If it ever needs re-fetching, take it from there.

The `35` is a halftone grade, not a weight: the cut is screened at 35 lpi so the
glyphs read as a document degraded by photocopying, which is the entire point of
the typeface. The package also holds grades 10/20/50/70/100 and Bold + Italic of
each, if the design ever wants them. A plain serif fallback loses the meaning,
not just the look.

Worth knowing: **Commit Mono is not on Google Fonts** — that request 404s. The
Figma Make file's `@import` for it was silently failing, so the design was
previewed with `ui-monospace` all along. The fallback stack here matches what
was actually on screen when the design was signed off.

## The device count says streams, and means streams

`presence` reports how many streams the room is holding. The UI says "2
devices" because that is the question people are asking, but two tabs on one
laptop count twice and there is no way to tell them apart — nor should there
be, since that would mean identifying devices.

It leaks nothing. The server was already holding this number, and it is the
size of your own room: not the content, not who, not where. `internal/hub`
still never reads content, and this did not change that.

The count is hidden whenever the stream is not live, in `setStatus`. "2
devices" next to "Offline" asserts something the client has stopped knowing.
It returns on its own, because reconnecting re-subscribes and subscribing is
what makes the server send a count.

## Settled: rotating a room empties the old one

"New room" in the pairing sheet is the only answer to *that link went somewhere
it should not have*, and the README's threat model needs one — the key is in
the URL, so a leaked URL is a leaked room, and before this there was no remedy
short of clearing browser storage on every device by hand.

**It clears the old room before leaving it.** Minting a new room on its own is
not a remedy: the scraps in the old one stay readable at the old URL for up to
24h, which is the entire reason someone is pressing the button.

**It cannot evict the other devices, and the sheet says so before you press.**
They hold the old key, and nothing distinguishes them from the person pressing
the button — there are no accounts here, which is the point. They see the room
empty out and the count fall, and they need pairing again.

It takes two presses. There is no undo, and the label changing to a question is
the confirmation; it disarms itself after five seconds, because a button left
reading "Throw it all away?" is a trap for the next person who opens the sheet.
The sheet's `close` handler disarms it too.

The warning text is hidden until the button arms, and **follows the button in
the DOM** so `.action-reset[data-confirming] + .pair-reset-note` can reveal it.
Reorder them and the warning stops appearing at the one moment it matters.

### The sheet has to be able to scroll

This shipped broken once and is worth knowing about. `.sheet` is
`position: fixed`, so with `overflow: visible` anything past the viewport is not
merely off screen — it is unreachable. No scrollbar, no gesture, nothing.

The sheet holds a 12rem QR, the code, a countdown, the join form and the reset,
which comes to within a few rem of a phone's usable height. Adding the reset
section pushed the bottom off the end, and the button was invisible and
unclickable while the markup, the handler and the deploy were all correct. The
symptom was "the button does nothing", and every place you would look to debug
that was fine.

`.sheet` therefore carries `max-height`, `overflow-y: auto` and **padding**.
The padding is load-bearing: scrolling one axis clips the other, and
`.sheet-body::before` paints outside its box in both — the rough edge by a
couple of px, the drop shadow by considerably more. Without room to land in,
turning on scrolling slices the paper's torn edge off square.

**The scrollbar is hidden, the scrolling is not** — `scrollbar-width: none`
plus a `::-webkit-scrollbar` rule. A gutter down the side of a torn paper sheet
is the one piece of browser chrome this design cannot absorb, and wheel, touch
and keyboard all still work without it. The hidden bar is exactly why the rest
of this has to stay: someone who sees no scrollbar and concludes the sheet does
not scroll, and then drops `overflow-y`, puts the button out of reach again.
`web/layout_test.go` pins all of it together for that reason.

Rotation reuses `createRoom()`, so it inherits the jittered 429 backoff. Hammer
the button and you hit the 10-rooms/hour cap like anything else.

## Known gap: catch-up only adds

`visibilitychange` fetches items newer than the newest one held, which is what
the brief specifies. It cannot see deletions that happened while the tab was
away, so a scrap deleted elsewhere lingers until reload. Reconciling properly
means either fetching every item (up to 50 × 256 KB — far too much for a phone
waking on cellular) or a new IDs-only endpoint. Left alone deliberately; revisit
only if it actually bites.

## Layout

```
cmd/toss/main.go        server setup, graceful shutdown
internal/hub/hub.go     rooms, subscribers, fan-out   (sweeper: M2)
internal/hub/item.go    Item
internal/api/routes.go  handlers, security headers, room page
internal/api/sse.go     stream handler
internal/api/pair.go    short-code mint/claim/redeem
internal/api/limit.go   per-IP token buckets
internal/api/paircode_parity_test.go
                        pins normalizePairCode against crypto.js
web/embed.go            embed.FS — the frontend ships inside the binary
web/manifest.webmanifest
                        installable to a home screen; start_url is "/"
web/icons/icon.svg      the only icon source; generate.sh derives the PNGs
web/index.html          semantic, unstyled
web/app.js              all behaviour
web/qr.js               QR encoder, so the code can carry the key fragment
web/crypto.js           AES-GCM, key handling, pairing-code wrap/unwrap
web/qr_test.go          runs qr.js under node, against go-qrcode
web/crypto_test.go      runs crypto.js under node, property by property
web/e2e_test.go         both devices, real server, typed-code pairing
web/manifest_test.go    pins start_url, and that no key can reach the manifest
web/app.css             the design, applied at M4
```

Script order in `index.html` is `qr.js`, `crypto.js`, `app.js`. All three are
`defer`, which runs them in document order; `app.js` reads `window.TossQR` and
`window.TossCrypto` at boot.

## Limits

| | | |
|---|---|---|
| Item TTL | 24h | swept every 60s, filtered on read |
| Items per room | 50 | oldest dropped first |
| Max item size | 256 KB | `http.MaxBytesReader` |
| Writes | 60/min per IP | sends, deletes, pairing |
| Room creation | 10/hr per IP | |
| Room sweep | untouched 48h | only with zero subscribers |
| Pairing code | 5 min, one redemption | |

`TOSS_TRUST_PROXY` to honour proxy headers, `TOSS_ADDR` to move off `:8080`.

## Frontend rules

- CSS hooks are classes, JS hooks are `data-*`. They don't overlap, so M4 can
  replace `app.css` without touching `app.js`. If you reach for a class name in
  the JS, add a data attribute instead.
- Send via the **document-level `paste` listener**, plus the compose field.
  Never `navigator.clipboard.readText()` — the page *pulling* from the
  clipboard is a permissioned operation (a system prompt every time on iOS, a
  grantable permission on Android, unavailable to pages on Firefox), whereas
  the user *pushing* text in is not gated at all. See "The compose field is not
  decorative" below.
- `localStorage` holds the room ID and key, as **one JSON entry** under
  `toss.room` so the two cannot drift apart. Never item content.
- **Plaintext lives in exactly two places**: the `text` field of a `rendered`
  entry, and the local variable it was decrypted into. Not in `localStorage`,
  not in an attribute, not in a log. `copyItem` reads `entry.text`, never
  `entry.item.content` — that one is ciphertext now.
- `upsert()` is **synchronous and takes already-decrypted text**. Anything from
  the network goes through `accept()`, which decrypts and then calls it. That
  ordering is what keeps the duplicate check at the top of `upsert` atomic with
  respect to the insert: two deliveries of one item can both clear the check in
  `accept`, and only one can clear the check in `upsert`.
- Optimistic echo: a pasted item renders immediately and reconciles when the
  POST returns. The UI never waits on the network.

State the JS already exposes for the design to style:

```
[data-role="status"][data-state="connecting|live|offline"]
[data-role="scrap"][data-age="fresh|fading|dying"]
[data-role="scrap"][data-pending]       optimistic, unacknowledged
[data-role="scrap"][data-copied]        2s after a successful copy
[data-role="scrap"][data-failed]        send failed
[data-role="scrap"][data-copy-failed]   clipboard write refused
[data-role="scrap"][data-undecryptable] arrived, but sealed under another key
[data-role="scrap"][data-long]          over 5 lines or 200 chars
[data-role="scrap"][data-expanded]      "Show all" was pressed
style="--rotation: -0.4deg"             seeded from the ULID, stable everywhere
```

`data-undecryptable` is usually not a fault — it is a scrap from before this
device was paired — so it is styled in the same voice as the rest of the chrome
rather than as an error, and its Copy button is disabled.

```
[data-role="paired"][data-show]         the PAIRED stamp, ~1.8s
[data-role="peers"][data-count="N"]      streams on the room; hidden unless live
[data-role="rotate"][data-confirming]    armed, waiting for the second press
```

Both devices show it: the one that offered the code gets a `paired` event over
the stream, the one that joined calls `showPaired()` directly. The joining
device needs `sessionStorage` for the typed path, because redeeming is followed
by a real navigation that would otherwise lose the fact. The stamp itself is
`aria-hidden`; the live region carries "Paired with iPhone · Safari", which is
the only place the cosmetic device label is worth saying out loud.

## Scope — do not build, do not scaffold for

Files, images, rich text, search, tags, accounts, teams, sharing with other
people, native apps, offline queue, notifications, analytics.

"Native apps" does not rule out the web app manifest — that is a link tag and a
static JSON file, and installing stays optional. It *does* rule out the service
worker that would normally come with it, which is the thing that would turn
into an offline queue. See "Settled: the manifest starts at `/`".

The binary's only dependency is `github.com/oklog/ulid/v2`.
`github.com/skip2/go-qrcode` is in `go.mod` but is **test-only** — it is the
reference `web/qr.js` is checked against, and nothing imports it outside
`_test.go`. Ask before adding anything else. No framework, no router, no ORM,
no build step.

No crypto dependency either, on either side: the Go half never touches
ciphertext, and the browser half is WebCrypto, which ships in the browser.

## Milestones — all done

- ~~**M1**~~ — Rooms in `localStorage`, `/r/<room>` routing, pairing code,
  QR, mobile layout, plus the two pieces of the design that need JS rather than
  CSS (seeded rotation, "Show all" expander), pulled forward from M4 so that M4
  stays a pure restyle.
- ~~**M2**~~ — Sweeper, rate limits, catch-up on `visibilitychange` and
  `pageshow`, `retry:` hint on the stream, and recovery when a room is gone.
- ~~**M3**~~ — E2EE. AES-GCM 256 via WebCrypto, fresh 96-bit IV per item, key
  in the URL fragment as `#k=<base64url>`. The QR needed nothing, exactly as
  planned — it already encoded `location.hash`.

  The one thing that did **not** go to plan: the typed-code path turned out to
  need a server change after all, because a server-chosen pairing code can
  unwrap the payload the server is storing. See "Settled: the pairing code is
  generated in the browser". `internal/hub` was not touched at all, which was
  the part that mattered.
- ~~**M4**~~ — done ahead of M3. The design is applied as CSS only: `app.css`
  was replaced wholesale and the static `#rough-edge` filter added to
  `index.html`. **`app.js` was not touched**, which was the whole point of
  pulling the seeded rotation and the expander forward into M1.

  Stack stayed vanilla. None of the Figma Make React/Tailwind/framer-motion code
  ships. Entry animation is `@starting-style` + transitions rather than springs.
  Still outstanding from the design: framer-motion's `layout` reflow (items
  sliding up when one above is deleted) — View Transitions would cover it.

## Before it goes live

What is left is deployment and one physical check, not code.

1. **HTTPS.** Non-negotiable since M3 — see "Run it". Without it the app shows
   "Needs HTTPS" and stops.
2. **Exactly one instance.** See the top of this file and the Dockerfile.
   **This rules out Vercel, and every other serverless host**: the platform runs
   N stateless invocations, a POST and an SSE stream land on different ones, and
   the failure is the silent one — 201 to the sender, nothing to the receiver.
   Long-lived SSE also outlives serverless execution limits. Use a container
   host that will hold one process: Fly.io (`min_machines_running = 1`, and turn
   autoscaling off), Render, Railway, or a VPS with Caddy in front.
3. **Scan the QR with a real phone camera, once.** `qr.js` is verified
   module-for-module against `go-qrcode` across all 20 versions, but no test
   here has put a symbol in front of an actual camera. Do it before launch.
4. **Decide what happens to `web/fonts/OFL-*.txt`** — they are embedded and
   served, which satisfies the OFL, but nothing links to them from the page.
~~5. **Add the app to an iPhone home screen, once.**~~ Done — it resumes the
   room Safari was in. See "Settled: the manifest starts at `/`".

Not blockers, and both deliberate: catch-up only adds and cannot see deletions
made while a tab was away; the layout reflow on delete is still missing.
