// QR encoding, in the browser.
//
// This exists because of encryption, not convenience. From M3 the room key
// lives in the URL fragment, and the QR has to contain that fragment for a scan
// to be enough to pair. A server-rendered QR would mean handing the key to the
// server -- the exact thing the fragment is there to prevent. So the code is
// drawn here, where the key already is.
//
// Byte mode, error correction level M, versions 1-20. That is everything a room
// URL needs and nothing more.
//
// Verified against github.com/skip2/go-qrcode by web/qr_test.go: given the same
// input and the same mask, both produce a byte-identical module matrix, across
// every version. Run it if you change anything in here -- the tables are
// unforgiving and a wrong entry produces a code that still *looks* like a QR
// and does not scan.
//
// The two encoders do disagree on which mask to *pick*, on roughly a fifth of
// inputs. That is a quality heuristic, not correctness -- any mask decodes --
// and the disagreement is go-qrcode's: its rule 1 scores a run of exactly five
// as zero where the spec scores it 3. Hence the test pins the mask.

window.TossQR = (() => {
  'use strict';

  // Per version: [total codewords, EC codewords per block,
  //               group1 blocks, group1 data codewords,
  //               group2 blocks, group2 data codewords]
  const VERSIONS = {
    1: [26, 10, 1, 16, 0, 0],
    2: [44, 16, 1, 28, 0, 0],
    3: [70, 26, 1, 44, 0, 0],
    4: [100, 18, 2, 32, 0, 0],
    5: [134, 24, 2, 43, 0, 0],
    6: [172, 16, 4, 27, 0, 0],
    7: [196, 18, 4, 31, 0, 0],
    8: [242, 22, 2, 38, 2, 39],
    9: [292, 22, 3, 36, 2, 37],
    10: [346, 26, 4, 43, 1, 44],
    11: [404, 30, 1, 50, 4, 51],
    12: [466, 22, 6, 36, 2, 37],
    13: [532, 22, 8, 37, 1, 38],
    14: [581, 24, 4, 40, 5, 41],
    15: [655, 24, 5, 41, 5, 42],
    16: [733, 28, 7, 45, 3, 46],
    17: [815, 28, 10, 46, 1, 47],
    18: [901, 26, 9, 43, 4, 44],
    19: [991, 26, 3, 44, 11, 45],
    20: [1085, 26, 3, 41, 13, 42],
  };

  // Centres of the alignment patterns, per version.
  const ALIGN = {
    1: [], 2: [6, 18], 3: [6, 22], 4: [6, 26], 5: [6, 30], 6: [6, 34],
    7: [6, 22, 38], 8: [6, 24, 42], 9: [6, 26, 46], 10: [6, 28, 50],
    11: [6, 30, 54], 12: [6, 32, 58], 13: [6, 34, 62], 14: [6, 26, 46, 66],
    15: [6, 26, 48, 70], 16: [6, 26, 50, 74], 17: [6, 30, 54, 78],
    18: [6, 30, 56, 82], 19: [6, 30, 58, 86], 20: [6, 34, 62, 90],
  };

  // --- GF(256) arithmetic, for Reed-Solomon ---

  const EXP = new Uint8Array(512);
  const LOG = new Uint8Array(256);
  (() => {
    let x = 1;
    for (let i = 0; i < 255; i++) {
      EXP[i] = x;
      LOG[x] = i;
      x <<= 1;
      if (x & 0x100) x ^= 0x11d; // the QR generator polynomial
    }
    for (let i = 255; i < 512; i++) EXP[i] = EXP[i - 255];
  })();

  const mul = (a, b) => (a === 0 || b === 0 ? 0 : EXP[LOG[a] + LOG[b]]);

  // Generator polynomial for `degree` error correction codewords.
  function generator(degree) {
    let poly = [1];
    for (let i = 0; i < degree; i++) {
      const next = new Array(poly.length + 1).fill(0);
      for (let j = 0; j < poly.length; j++) {
        next[j] ^= poly[j];
        next[j + 1] ^= mul(poly[j], EXP[i]);
      }
      poly = next;
    }
    return poly;
  }

  function ecCodewords(data, count) {
    const gen = generator(count);
    const rem = new Array(count).fill(0);
    for (const byte of data) {
      const factor = byte ^ rem[0];
      rem.shift();
      rem.push(0);
      for (let i = 0; i < count; i++) rem[i] ^= mul(gen[i + 1], factor);
    }
    return rem;
  }

  // --- bit stream ---

  class Bits {
    constructor() {
      this.bits = [];
    }
    push(value, length) {
      for (let i = length - 1; i >= 0; i--) this.bits.push((value >> i) & 1);
    }
    get length() {
      return this.bits.length;
    }
    toBytes() {
      const out = [];
      for (let i = 0; i < this.bits.length; i += 8) {
        let byte = 0;
        for (let j = 0; j < 8; j++) byte = (byte << 1) | (this.bits[i + j] || 0);
        out.push(byte);
      }
      return out;
    }
  }

  const dataCapacity = (v) => {
    const [, ec, g1b, g1d, g2b, g2d] = VERSIONS[v];
    void ec;
    return g1b * g1d + g2b * g2d;
  };

  function pickVersion(byteLength) {
    for (let v = 1; v <= 20; v++) {
      // 4 bits mode + 8 or 16 bits length + the data itself.
      const header = 4 + (v <= 9 ? 8 : 16);
      if (dataCapacity(v) * 8 >= header + byteLength * 8) return v;
    }
    throw new Error('too much data for a version 20 QR code');
  }

  // --- data encoding ---

  function encodeData(bytes, version) {
    const [, ecPerBlock, g1b, g1d, g2b, g2d] = VERSIONS[version];
    const capacity = dataCapacity(version);

    const bits = new Bits();
    bits.push(0b0100, 4); // byte mode
    bits.push(bytes.length, version <= 9 ? 8 : 16);
    for (const b of bytes) bits.push(b, 8);

    // Terminator, up to four zero bits.
    const room = capacity * 8 - bits.length;
    bits.push(0, Math.min(4, room));
    // Pad to a byte boundary.
    while (bits.length % 8 !== 0) bits.push(0, 1);

    const codewords = bits.toBytes();
    // Then the two alternating pad codewords, forever.
    const PAD = [0xec, 0x11];
    for (let i = 0; codewords.length < capacity; i++) codewords.push(PAD[i % 2]);

    // Split into blocks, each with its own error correction.
    const blocks = [];
    let at = 0;
    for (let i = 0; i < g1b; i++) {
      blocks.push(codewords.slice(at, at + g1d));
      at += g1d;
    }
    for (let i = 0; i < g2b; i++) {
      blocks.push(codewords.slice(at, at + g2d));
      at += g2d;
    }
    const ecBlocks = blocks.map((b) => ecCodewords(b, ecPerBlock));

    // Interleave: one codeword from each block in turn, data then EC.
    const out = [];
    const maxData = Math.max(...blocks.map((b) => b.length));
    for (let i = 0; i < maxData; i++) {
      for (const b of blocks) if (i < b.length) out.push(b[i]);
    }
    for (let i = 0; i < ecPerBlock; i++) {
      for (const b of ecBlocks) out.push(b[i]);
    }
    return out;
  }

  // --- matrix ---

  const FINDER = [
    [1, 1, 1, 1, 1, 1, 1],
    [1, 0, 0, 0, 0, 0, 1],
    [1, 0, 1, 1, 1, 0, 1],
    [1, 0, 1, 1, 1, 0, 1],
    [1, 0, 1, 1, 1, 0, 1],
    [1, 0, 0, 0, 0, 0, 1],
    [1, 1, 1, 1, 1, 1, 1],
  ];

  function buildMatrix(version) {
    const size = version * 4 + 17;
    const m = Array.from({ length: size }, () => new Array(size).fill(null));
    const reserved = Array.from({ length: size }, () => new Array(size).fill(false));

    const place = (top, left, pattern) => {
      for (let r = 0; r < pattern.length; r++) {
        for (let c = 0; c < pattern.length; c++) {
          m[top + r][left + c] = pattern[r][c];
          reserved[top + r][left + c] = true;
        }
      }
    };

    // Three finder patterns, each with a one-module separator.
    for (const [top, left] of [[0, 0], [0, size - 7], [size - 7, 0]]) {
      place(top, left, FINDER);
      for (let i = -1; i <= 7; i++) {
        for (const [r, c] of [[top + i, left - 1], [top + i, left + 7], [top - 1, left + i], [top + 7, left + i]]) {
          if (r >= 0 && r < size && c >= 0 && c < size && !reserved[r][c]) {
            m[r][c] = 0;
            reserved[r][c] = true;
          }
        }
      }
    }

    // Timing patterns.
    for (let i = 8; i < size - 8; i++) {
      if (!reserved[6][i]) { m[6][i] = i % 2 === 0 ? 1 : 0; reserved[6][i] = true; }
      if (!reserved[i][6]) { m[i][6] = i % 2 === 0 ? 1 : 0; reserved[i][6] = true; }
    }

    // Alignment patterns, skipping any that would collide with a finder.
    const centres = ALIGN[version];
    for (const r of centres) {
      for (const c of centres) {
        if ((r <= 8 && c <= 8) || (r <= 8 && c >= size - 9) || (r >= size - 9 && c <= 8)) continue;
        for (let dr = -2; dr <= 2; dr++) {
          for (let dc = -2; dc <= 2; dc++) {
            m[r + dr][c + dc] = Math.max(Math.abs(dr), Math.abs(dc)) !== 1 ? 1 : 0;
            reserved[r + dr][c + dc] = true;
          }
        }
      }
    }

    // The dark module, always set.
    m[size - 8][8] = 1;
    reserved[size - 8][8] = true;

    // Reserve the format information areas.
    for (let i = 0; i < 9; i++) {
      if (!reserved[8][i]) { reserved[8][i] = true; m[8][i] = 0; }
      if (!reserved[i][8]) { reserved[i][8] = true; m[i][8] = 0; }
    }
    for (let i = 0; i < 8; i++) {
      if (!reserved[8][size - 1 - i]) { reserved[8][size - 1 - i] = true; m[8][size - 1 - i] = 0; }
      if (!reserved[size - 1 - i][8]) { reserved[size - 1 - i][8] = true; m[size - 1 - i][8] = 0; }
    }

    // Version information, version 7 and up.
    if (version >= 7) {
      const bits = versionBits(version);
      for (let i = 0; i < 18; i++) {
        const bit = (bits >> i) & 1;
        const r = Math.floor(i / 3);
        const c = size - 11 + (i % 3);
        m[r][c] = bit; reserved[r][c] = true;
        m[c][r] = bit; reserved[c][r] = true;
      }
    }

    return { m, reserved, size };
  }

  // 18-bit BCH for the version information.
  function versionBits(version) {
    let d = version << 12;
    for (let i = 0; i < 6; i++) {
      if (d & (1 << (17 - i))) d ^= 0x1f25 << (5 - i);
    }
    return (version << 12) | d;
  }

  // 15-bit BCH for the format information. EC level M is 0b00.
  function formatBits(mask) {
    const data = (0b00 << 3) | mask;
    let d = data << 10;
    for (let i = 0; i < 5; i++) {
      if (d & (1 << (14 - i))) d ^= 0x537 << (4 - i);
    }
    return ((data << 10) | d) ^ 0x5412;
  }

  function placeFormat(m, size, mask) {
    const bits = formatBits(mask);
    const bit = (i) => (bits >> i) & 1;

    // Around the top-left finder: bits 0-5 down column 8, the three that
    // straddle the timing patterns, then bits 9-14 back along row 8.
    for (let i = 0; i <= 5; i++) m[i][8] = bit(i);
    m[7][8] = bit(6);
    m[8][8] = bit(7);
    m[8][7] = bit(8);
    for (let i = 9; i <= 14; i++) m[8][14 - i] = bit(i);

    // Duplicated beside the other two finders. Bits 0-7 run along row 8 from
    // the right edge; bits 8-14 go up column 8 from the bottom. Note the
    // asymmetry: it is 8 then 7, because taking eight up the column would land
    // bit 7 on (size-8, 8), which is the dark module and must stay set.
    for (let i = 0; i <= 7; i++) m[8][size - 1 - i] = bit(i);
    for (let i = 8; i <= 14; i++) m[size - 15 + i][8] = bit(i);
  }

  // Data snakes up and down in two-module-wide columns, right to left.
  function placeData(m, reserved, size, codewords) {
    let bitIndex = 0;
    const nextBit = () => {
      const byte = codewords[bitIndex >> 3];
      const bit = byte === undefined ? 0 : (byte >> (7 - (bitIndex & 7))) & 1;
      bitIndex++;
      return bit;
    };

    let upward = true;
    for (let right = size - 1; right >= 1; right -= 2) {
      if (right === 6) right = 5; // the vertical timing pattern is skipped
      for (let i = 0; i < size; i++) {
        const row = upward ? size - 1 - i : i;
        for (const col of [right, right - 1]) {
          if (!reserved[row][col]) m[row][col] = nextBit();
        }
      }
      upward = !upward;
    }
  }

  const MASKS = [
    (r, c) => (r + c) % 2 === 0,
    (r) => r % 2 === 0,
    (r, c) => c % 3 === 0,
    (r, c) => (r + c) % 3 === 0,
    (r, c) => (Math.floor(r / 2) + Math.floor(c / 3)) % 2 === 0,
    (r, c) => ((r * c) % 2) + ((r * c) % 3) === 0,
    (r, c) => (((r * c) % 2) + ((r * c) % 3)) % 2 === 0,
    (r, c) => (((r + c) % 2) + ((r * c) % 3)) % 2 === 0,
  ];

  function applyMask(m, reserved, size, mask) {
    const out = m.map((row) => row.slice());
    for (let r = 0; r < size; r++) {
      for (let c = 0; c < size; c++) {
        if (!reserved[r][c] && MASKS[mask](r, c)) out[r][c] ^= 1;
      }
    }
    return out;
  }

  // The four penalty rules from the spec. Lowest total wins.
  function penalty(m, size) {
    let score = 0;

    // Rule 1: runs of five or more of the same colour.
    for (let i = 0; i < size; i++) {
      for (const line of [m[i], m.map((row) => row[i])]) {
        let run = 1;
        for (let j = 1; j < size; j++) {
          if (line[j] === line[j - 1]) {
            run++;
            if (run === 5) score += 3;
            else if (run > 5) score += 1;
          } else run = 1;
        }
      }
    }

    // Rule 2: 2x2 blocks of one colour.
    for (let r = 0; r < size - 1; r++) {
      for (let c = 0; c < size - 1; c++) {
        const v = m[r][c];
        if (v === m[r][c + 1] && v === m[r + 1][c] && v === m[r + 1][c + 1]) score += 3;
      }
    }

    // Rule 3: finder-like patterns anywhere else.
    const A = [1, 0, 1, 1, 1, 0, 1, 0, 0, 0, 0];
    const B = [0, 0, 0, 0, 1, 0, 1, 1, 1, 0, 1];
    const matches = (line, at, pat) => pat.every((v, k) => line[at + k] === v);
    for (let i = 0; i < size; i++) {
      const row = m[i];
      const col = m.map((r) => r[i]);
      for (let j = 0; j + 11 <= size; j++) {
        for (const line of [row, col]) {
          if (matches(line, j, A) || matches(line, j, B)) score += 40;
        }
      }
    }

    // Rule 4: deviation from an even split of dark and light.
    let dark = 0;
    for (const row of m) for (const v of row) dark += v;
    const percent = (dark * 100) / (size * size);
    score += Math.floor(Math.abs(percent - 50) / 5) * 10;

    return score;
  }

  /**
   * encode(text) -> { size, modules }
   * modules is a size x size array of 0/1, 1 being a dark module.
   */
  function encode(text, { forceMask = null } = {}) {
    const bytes = Array.from(new TextEncoder().encode(text));
    const version = pickVersion(bytes.length);
    const codewords = encodeData(bytes, version);

    const { m, reserved, size } = buildMatrix(version);
    placeData(m, reserved, size, codewords);

    const build = (mask) => {
      const candidate = applyMask(m, reserved, size, mask);
      placeFormat(candidate, size, mask);
      return candidate;
    };

    // forceMask exists for the conformance test against go-qrcode: mask choice
    // is a quality heuristic, so two correct encoders can disagree on it while
    // producing equally valid codes. Pinning it compares everything else.
    if (forceMask !== null) {
      return { size, modules: build(forceMask), version, mask: forceMask };
    }

    let best = null;
    for (let mask = 0; mask < 8; mask++) {
      const modules = build(mask);
      const score = penalty(modules, size);
      if (!best || score < best.score) best = { score, mask, modules };
    }
    return { size, modules: best.modules, version, mask: best.mask };
  }

  /** Renders to a crisp data: URL, one <img> away from being on screen. */
  function toDataURL(text, { moduleSize = 8, quiet = 4 } = {}) {
    const { size, modules } = encode(text);
    const dim = (size + quiet * 2) * moduleSize;

    let rects = '';
    for (let r = 0; r < size; r++) {
      for (let c = 0; c < size; c++) {
        if (modules[r][c]) {
          rects += `<rect x="${(c + quiet) * moduleSize}" y="${(r + quiet) * moduleSize}" width="${moduleSize}" height="${moduleSize}"/>`;
        }
      }
    }
    const svg =
      `<svg xmlns="http://www.w3.org/2000/svg" width="${dim}" height="${dim}" viewBox="0 0 ${dim} ${dim}" shape-rendering="crispEdges">` +
      `<rect width="${dim}" height="${dim}" fill="#fff"/><g fill="#000">${rects}</g></svg>`;

    return 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg);
  }

  // _internals is exposed so the conformance test can rebuild the function-
  // pattern map and read a foreign QR code back. Nothing in the app uses it.
  return {
    encode,
    toDataURL,
    _internals: { VERSIONS, ALIGN, buildMatrix, MASKS, encodeData, ecCodewords, generator },
  };
})();
