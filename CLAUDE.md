# Toss

Move text between your own devices. Paste on one, it appears on the others.
No accounts, no install, no native app.

**Status: M4 applied, M3 outstanding.** Transport, rooms, pairing, expiry,
limits, recovery and the design are all in. The one thing still missing is
end-to-end encryption, which is client-side only and touches no markup.

## Run it

```sh
go run ./cmd/toss              # :8080, or set TOSS_ADDR
go test -race ./...            # the hub is concurrent; always use -race
```

Open `/`. It mints a room, rewrites to `/r/<room>` and remembers it in
`localStorage`. "Add device" shows a QR and an 8-character code; another device
scans, or types the code into the same sheet, and lands in the same room.

`localhost` counts as a secure context, so tap-to-copy works in dev over plain
HTTP. Any other host needs real HTTPS or `navigator.clipboard.writeText()` will
refuse. Set `TOSS_ORIGIN` if a proxy rewrites the host, or the QR will point at
the wrong place.

## The one thing that will silently break this

**Exactly one process.** All state is in memory and there is no cross-process
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
  logs it. From M3 it is AES-GCM ciphertext and the key lives in the URL
  fragment, which browsers never send. Nothing in Go changes at M3 **as long as
  nothing here ever reads content.** Don't write that code.

## Unresolved: the QR cannot carry the key

This has to be decided before M3, and it is a product call, not a technical one.

The QR is generated server-side by `go-qrcode`, so **anything encoded in it is
something the server knows**. Today it encodes `<origin>/r/<room>` and no key
exists yet, so it is fine. At M3 the key lives in the URL fragment precisely so
the server can never see it — which means the server cannot put it in a QR.
Generating the QR from a client-supplied URL would hand over the key and undo
the whole design.

So a scan alone cannot complete pairing under E2EE. Two ways out:

1. **Move QR generation into the browser.** The scan keeps working exactly as it
   does now. Costs a hand-written QR encoder in vanilla JS (no build step, no
   dependency allowed) and `go-qrcode` goes away.
2. **Split the roles.** The scan gets you into the room; the key rides the typed
   code. `POST /api/pair` already accepts an opaque `payload` for this: device A
   wraps the room key under a secret derived from the code, the server stores
   only ciphertext, device B derives the same secret from what was typed and
   unwraps locally. No server change needed — that is why the field exists.

Option 2 needs no new code on the Go side but makes the QR a convenience rather
than a complete handshake. Option 1 keeps the product promise intact and costs
a few hundred lines of JS.

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
- Pairing codes are single-redemption and expire in 5 minutes. 40 bits is not
  much, so the window and the one-guess-per-code rule are what make it safe.
  Redeem failures return one message for "unknown", "used" and "expired" alike;
  telling them apart tells an attacker which guesses were close.
- `GET /r/{room}` does **not** check the room exists. At M3 the key is in the
  fragment, which never reaches the server, so the client has to be running
  before it can tell. A stale room is the client's problem, and it recovers by
  minting a new one.
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

## Watch out: restart stampede

Recovery-from-a-missing-room (M2) and the 10 rooms/hr per-IP cap interact
badly. When the server restarts, **every open tab** discovers its room is gone
and mints a new one. Behind a single NAT, only 10 succeed in the hour; the rest
sit on "No room" until the budget refills. This showed up for real while testing
— abandoned browser tabs exhausted the budget and starved later runs.

Not fixed, because the right fix depends on deployment: a higher cap, a shared
bucket exemption for recovery, or having the client back off and retry rather
than mint immediately. Worth deciding before toss.tools sees office traffic.

## Fonts

Self-hosted in `web/fonts/`, embedded in the binary. The CSP forbids external
hosts, and a font request to Google is a third party learning who reads a room.

| | |
|---|---|
| **Instrument Sans** | **Bundled.** OFL, see `fonts/OFL-InstrumentSans.txt`. One variable file covers 400–700. |
| **Redaction** | **Not bundled.** Only obtainable from a third-party CDN mirror with no licence file. Not going into a public repo unverified. |
| **Commit Mono** | **Not bundled.** Licence unverified from here. |

To add either: drop the woff2 into `web/fonts/` and uncomment its `@font-face`
at the top of `app.css`. Until then `--font-display` and `--font-mono` fall
through to sensible stacks and the layout is unaffected.

Worth knowing: **Commit Mono is not on Google Fonts** — that request 404s. The
Figma Make file's `@import` for it was silently failing, so the design was
previewed with `ui-monospace` all along. The fallback stack here matches what
was actually on screen when the design was signed off.

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
internal/api/routes.go  handlers, security headers, room page, QR
internal/api/sse.go     stream handler
internal/api/pair.go    short-code mint/redeem
internal/api/limit.go   per-IP token buckets
web/embed.go            embed.FS — the frontend ships inside the binary
web/index.html          semantic, unstyled
web/app.js              all behaviour
web/app.css             placeholder, replaced wholesale at M4
```

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

`TOSS_TRUST_PROXY` to honour proxy headers, `TOSS_ORIGIN` to fix the QR's
absolute URL, `TOSS_ADDR` to move off `:8080`.

## Frontend rules

- CSS hooks are classes, JS hooks are `data-*`. They don't overlap, so M4 can
  replace `app.css` without touching `app.js`. If you reach for a class name in
  the JS, add a data attribute instead.
- Send via the **document-level `paste` listener**. Never
  `navigator.clipboard.readText()` — it prompts. The paste event doesn't, on
  any browser including iOS.
- `localStorage` will hold the room ID and key (M1/M3). Never item content.
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
[data-role="scrap"][data-long]          over 5 lines or 200 chars
[data-role="scrap"][data-expanded]      "Show all" was pressed
style="--rotation: -0.4deg"             seeded from the ULID, stable everywhere
```

**Gap in the supplied design:** it draws the pairing sheet that *shows* a code,
but there is no design for the half that *accepts* one. `.pair-join` in
`index.html` is unstyled scaffolding — it needs a real design before M4.

## Scope — do not build, do not scaffold for

Files, images, rich text, search, tags, accounts, teams, sharing with other
people, native apps, offline queue, notifications, analytics.

Dependencies are `github.com/oklog/ulid/v2` and (from M1)
`github.com/skip2/go-qrcode`. Ask before adding anything else. No framework,
no router, no ORM, no build step.

## Remaining milestones

- ~~**M1**~~ — done. Rooms in `localStorage`, `/r/<room>` routing, pairing code,
  QR, mobile layout, plus the two pieces of the design that need JS rather than
  CSS (seeded rotation, "Show all" expander), pulled forward from M4 so that M4
  stays a pure restyle.
- ~~**M2**~~ — done. Sweeper, rate limits, catch-up on `visibilitychange` and
  `pageshow`, `retry:` hint on the stream, and recovery when a room is gone.
- **M3** — E2EE. AES-GCM 256 via WebCrypto, fresh 96-bit IV per item, key in
  the URL fragment as `#k=<base64url>`. Client-side only.
- ~~**M4**~~ — done, ahead of M3. The design is applied as CSS only: `app.css`
  was replaced wholesale and the static `#rough-edge` filter added to
  `index.html`. **`app.js` was not touched**, which was the whole point of
  pulling the seeded rotation and the expander forward into M1.

  Stack stayed vanilla. None of the Figma Make React/Tailwind/framer-motion code
  ships. Entry animation is `@starting-style` + transitions rather than springs.
  Still outstanding from the design: framer-motion's `layout` reflow (items
  sliding up when one above is deleted) — View Transitions would cover it.
