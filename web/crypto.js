// Toss -- client-side encryption (M3).
//
// AES-GCM 256. The key is generated in the browser, lives in the URL fragment,
// and is never transmitted: fragments are not sent in HTTP requests, so the
// server stores ciphertext it has no way to read. Nothing in Go changes for
// this to hold -- what makes it true is that the server never reads content,
// and that the key never leaves the fragment.
//
// Two ways a second device gets the key:
//
//   scanning  the QR encodes location.hash, so the key rides along in the code
//             and never touches the network at all
//   typing    the key is wrapped under a secret derived from the pairing code
//             and parked on the server as an opaque blob
//
// The typed path is why newPairCode() is here rather than on the server. The
// payload is wrapped under the code; if the server chose the code, the server
// would know the secret that unwraps the payload it is holding, and could read
// the room. Anything the server picks, the server knows -- so the client picks.
//
// Verified by web/crypto_test.go, which drives this file under node.

(() => {
  'use strict';

  const KEY_ALGO = { name: 'AES-GCM', length: 256 };

  // 96 bits. AES-GCM is specified for a 96-bit IV; other lengths get hashed
  // down to one and lose the guarantee that distinct IVs stay distinct.
  const IV_BYTES = 12;

  const SALT_BYTES = 16;

  // The pairing code is 40 bits. That is brute-forceable offline by anyone
  // holding the wrapped payload -- which the server is, for up to five minutes
  // -- so the derivation has to be expensive enough to price 2^40 guesses out
  // of reach. At 310k iterations that is ~3e17 SHA-256 compressions per room.
  // This runs once per pairing, on a phone, so it is allowed to take a moment.
  const PBKDF2_ITERATIONS = 310000;

  // Crockford base32, matching pairAlphabet in internal/api/pair.go.
  const PAIR_ALPHABET = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
  const PAIR_CODE_LEN = 8;

  const enc = new TextEncoder();
  const dec = new TextDecoder();

  // --- base64url ---

  function toB64(bytes) {
    const b = new Uint8Array(bytes);
    let s = '';
    for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
    return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function fromB64(s) {
    let t = String(s).replace(/-/g, '+').replace(/_/g, '/');
    // atob tolerates missing padding in practice but is not specified to, and
    // a length of 4n+1 is invalid however it is padded.
    if (t.length % 4 === 1) throw new Error('malformed base64url');
    while (t.length % 4) t += '=';
    const bin = atob(t);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  // --- the room key ---

  const generateKey = () => crypto.subtle.generateKey(KEY_ALGO, true, ['encrypt', 'decrypt']);

  // Extractable, deliberately: the key has to be exportable to go in the
  // fragment, in the QR, and into the wrapped pairing payload. It is a secret
  // the user is expected to be able to carry to another device.
  const importKey = (b64) =>
    crypto.subtle.importKey('raw', fromB64(b64), KEY_ALGO, true, ['encrypt', 'decrypt']);

  const exportKey = async (key) => toB64(await crypto.subtle.exportKey('raw', key));

  // --- items ---

  // Fresh IV per item. Reusing one under the same key is the single way to
  // break GCM outright, and items are cheap, so there is no reuse anywhere.
  async function encrypt(key, text) {
    const iv = crypto.getRandomValues(new Uint8Array(IV_BYTES));
    const ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, key, enc.encode(text));
    return { iv: toB64(iv), content: toB64(ct) };
  }

  // Throws if the key is wrong, the IV is wrong, or a byte moved. GCM cannot
  // tell those apart, so neither can the caller.
  async function decrypt(key, iv, content) {
    const pt = await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv: fromB64(iv) },
      key,
      fromB64(content)
    );
    return dec.decode(pt);
  }

  // --- pairing codes ---

  // Rejection-free because 32 divides 256 exactly, so masking the low 5 bits is
  // uniform over the alphabet. Same argument as newPairCode in pair.go.
  function newPairCode() {
    const b = crypto.getRandomValues(new Uint8Array(PAIR_CODE_LEN));
    let out = '';
    for (let i = 0; i < b.length; i++) out += PAIR_ALPHABET[b[i] & 31];
    return out;
  }

  // Mirror of normalizePairCode in internal/api/pair.go, and it has to stay
  // one: the server uses its version to find the room, this one derives the key
  // that unwraps it. If they disagree, the code appears to work -- the right
  // room comes back -- and then nothing decrypts.
  //
  // Uppercasing is ASCII-only, on purpose. Go's strings.ToUpper is Unicode-
  // aware and the two disagree in both directions ('ß' uppercases to 'SS' in
  // JS and stays put in Go; 'ſ' becomes 'S' in Go and stays put here). Neither
  // is reachable from a phone keyboard typing an 8-character code, and pinning
  // this to ASCII is what makes the parity testable rather than approximate.
  // web/crypto_test.go checks all 128 ASCII code points against the Go side.
  function normalizeCode(s) {
    let out = '';
    for (const raw of String(s)) {
      const ch = raw >= 'a' && raw <= 'z' ? raw.toUpperCase() : raw;
      if (ch === 'O') out += '0';
      else if (ch === 'I' || ch === 'L') out += '1';
      else if (ch === '-' || ch === ' ') continue;
      else if (PAIR_ALPHABET.includes(ch)) out += ch;
    }
    return out;
  }

  // Groups the code for display: K3N8-XQ2M.
  const formatCode = (code) =>
    code.length === PAIR_CODE_LEN ? `${code.slice(0, 4)}-${code.slice(4)}` : code;

  // --- wrapping the room key under a pairing code ---

  async function codeKey(code, salt) {
    const material = await crypto.subtle.importKey(
      'raw',
      enc.encode(normalizeCode(code)),
      'PBKDF2',
      false,
      ['deriveKey']
    );
    return crypto.subtle.deriveKey(
      { name: 'PBKDF2', salt, iterations: PBKDF2_ITERATIONS, hash: 'SHA-256' },
      material,
      KEY_ALGO,
      false,
      ['encrypt', 'decrypt']
    );
  }

  // salt.iv.ciphertext, base64url. Opaque to the server, which stores it and
  // hands it back on redemption without ever being able to open it.
  async function wrapForCode(code, key) {
    const salt = crypto.getRandomValues(new Uint8Array(SALT_BYTES));
    const iv = crypto.getRandomValues(new Uint8Array(IV_BYTES));
    const wrapping = await codeKey(code, salt);
    const raw = await crypto.subtle.exportKey('raw', key);
    const ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, wrapping, raw);
    return `${toB64(salt)}.${toB64(iv)}.${toB64(ct)}`;
  }

  async function unwrapWithCode(code, payload) {
    const parts = String(payload).split('.');
    if (parts.length !== 3) throw new Error('malformed pairing payload');
    const [salt, iv, ct] = parts;
    const wrapping = await codeKey(code, fromB64(salt));
    const raw = await crypto.subtle.decrypt(
      { name: 'AES-GCM', iv: fromB64(iv) },
      wrapping,
      fromB64(ct)
    );
    return crypto.subtle.importKey('raw', raw, KEY_ALGO, true, ['encrypt', 'decrypt']);
  }

  window.TossCrypto = {
    generateKey,
    importKey,
    exportKey,
    encrypt,
    decrypt,
    newPairCode,
    normalizeCode,
    formatCode,
    wrapForCode,
    unwrapWithCode,
    _internals: { toB64, fromB64, PAIR_ALPHABET, PAIR_CODE_LEN },
  };
})();
