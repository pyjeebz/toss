# Toss

Move text between your own devices. Paste on one, it appears on the others.

No accounts, no app store, no database, nothing you have to install. The
content is encrypted in your browser, and the key never reaches the server.

**Live at [toss.pyjeebz.com](https://toss.pyjeebz.com).**

## Using it

Open it. That's the whole setup — you get a room and an encryption key
immediately, and the URL becomes `/r/<room>#k=<key>`.

**To send:** paste anywhere on the page (⌘V / Ctrl+V), or tap the field and
paste or type into it. On a phone the field is the way in, because touch
browsers only offer "Paste" when a text field has focus. A paste sends straight
away; typed text sends on Enter.

**To receive:** tap a scrap to copy it back to your clipboard. The status line
says how many devices are connected, so you can tell whether the other one is
actually listening.

**To add a device:** press **Add device** and scan the QR with the other
device's camera, or type the 8-character code into the same sheet there. Both
devices stamp PAIRED when it lands. The code lasts 5 minutes and works once.

Scraps expire after 24 hours on their own. You can throw one away sooner, or
clear the lot.

**To keep it handy:** add it to your home screen — "Add to Home Screen" on
iOS, "Install app" or "Add to Home screen" in Chrome's menu on Android. That
is a shortcut and nothing more: no download, no store, no background process,
and no offline cache. The app behaves identically in a tab, and you never have
to do it.

## What the server can and cannot see

This is the part worth being precise about.

Content is AES-GCM 256, encrypted and decrypted in your browser, with a fresh
96-bit IV per item. The key lives in the URL fragment — the `#k=...` part —
which browsers never transmit. So the server stores ciphertext it has no way to
open, and that isn't a policy, it's arithmetic.

| It sees | It never sees |
|---|---|
| Ciphertext and IVs | Anything you paste |
| How many bytes, and when | Your encryption key |
| Room IDs (random, tied to no identity) | |
| Your IP, for rate limiting — in memory, swept | |
| A cosmetic device label from your User-Agent | |
| Wrapped pairing payloads, for 5 minutes at most | |

Two ways a second device gets the key, neither of which hands it over:

- **Scanning** — the QR is drawn in your browser and encodes the whole URL,
  fragment included. The key never touches the network at all.
- **Typing** — the key is sealed under a secret derived from the pairing code
  and parked on the server as an opaque blob. The browser picks the code, not
  the server, for the obvious reason: a server that chose the code would know
  the secret that opens the blob it is holding.

### What this does not protect you from

- **Anyone with your URL has your scraps.** The key is in the link. Sending
  someone the address bar contents, or a photo of the QR, gives them the room.
  If that happens, **Add device → New room** empties the old room and moves you
  to a fresh one with a new key. Your other devices stay behind and need pairing
  again — they hold the old key, and nothing here can tell them apart from you.
- **There is no recovery.** No accounts means nothing to reset. Clear your
  browser storage and lose the link, and the room is gone for good.
- **A restart wipes everything.** Nothing is written to disk, deliberately.

It is designed to protect your text from the server and from anyone who gets
hold of what the server stores. It is not a secrets manager.

## How it works

Server-Sent Events, not WebSockets — `EventSource` gives reconnect and
`Last-Event-ID` replay for free, and phones suspend background tabs constantly,
so that path runs many times a day.

No database. Rooms, items and subscribers live in memory, and losing them on
restart is correct for content that expires in a day. The frontend is embedded
in the binary, so the binary is the entire artifact.

Room IDs are 120 bits of `crypto/rand`. Pairing codes are 8 typeable
characters that *resolve* to a room ID — deliberately a different thing, since
40 bits is brute-forceable and a room ID must not be.

## Running it locally

```sh
go run ./cmd/toss      # :8080, or set TOSS_ADDR
```

`localhost` counts as a secure context, so this works over plain HTTP.

**Anywhere else needs real HTTPS.** Browsers expose WebCrypto only in a secure
context, so over plain HTTP `crypto.subtle` is undefined and there is no
unencrypted mode to fall back to — the app says it needs a secure connection and
stops. Testing from a phone on your LAN needs a tunnel with TLS, not an IP
address.

## Deploying

```sh
docker build -t toss .
docker run -p 8080:8080 toss
```

**Run exactly one instance.** Every room, item and subscriber lives in one
process's memory and there is no cross-process fan-out. Two instances behind a
load balancer means a POST landing on one while the recipient's stream is held
by the other is delivered to nobody — no error, no log line, a 201 to the
sender. It presents as "toss is flaky", not as a misconfiguration.

That rules out serverless platforms: they run many stateless invocations by
design, and long-lived SSE streams outlive their execution limits.

`fly.toml` is included and pinned to one machine. Note that `fly deploy`
creates **two** machines by default, and `min_machines_running` does not
prevent it:

```sh
fly deploy --ha=false
```

To check any deployment from outside — a single process's room count can only
go up, so if this alternates, you have more than one machine:

```sh
for i in $(seq 6); do curl -s https://<app>/healthz; echo; done
```

| | |
|---|---|
| `TOSS_ADDR` | listen address, default `:8080` |
| `TOSS_TRUST_PROXY` | honour `X-Forwarded-For` for per-IP limits. Only set this behind a proxy you control — the header is client-supplied, so trusting it otherwise turns every rate limit into a suggestion. |

## Limits

| | |
|---|---|
| Item lifetime | 24 hours |
| Items per room | 50, oldest dropped first |
| Max item size | ~192 KB of text (256 KB on the wire) |
| Writes | 60/min per IP |
| Room creation | 10/hour per IP |
| Idle room sweep | 48 hours |

Reads are never rate limited. A phone waking up and reconnecting all day is the
product working correctly, not abuse.

The size cap applies to the encrypted request, which is base64 and so about a
third larger than your text — hence two numbers. It also counts bytes of UTF-8,
not characters, so text that is not mostly ASCII runs out sooner. Anything over
the line is refused outright and says so; nothing is silently truncated.

## Tests

```sh
go test -race ./...    # the hub is concurrent; always use -race
```

`web/qr.js` and `web/crypto.js` are hand-written and run under `node`, with
every pass/fail decision made in Go so a bug in the test driver cannot vote
itself correct.

The QR encoder is compared against a second implementation module-by-module, at
every version and on both sides of every capacity boundary — a wrong table entry
produces a code that still *looks* like a QR and simply does not scan, which is
a bug that reaches a phone camera before it reaches a person. The crypto is
checked by the properties whose failure is silent, plus an end-to-end run where
one device pairs by typed code and reads back what the other sent, asserting
that neither the plaintext nor the key appears in what the server serves.

Those tests skip without `node`, so the suite still passes — but then nothing is
checking either file.

## Notes

`CLAUDE.md` carries the design decisions, the things that look optional and are
load-bearing, and the traps. Worth reading before changing `internal/hub` or
anything on the pairing path.

The stack is deliberately plain: Go standard library plus `oklog/ulid`, and
vanilla JS with no build step, no framework and no bundler. `skip2/go-qrcode`
is in `go.mod` but is test-only — it is the reference the QR encoder is checked
against, and it is not linked into the binary.
