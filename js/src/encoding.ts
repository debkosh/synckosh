// Base64 without padding, standard and URL alphabets, as Go's encoding/base64 RawStdEncoding and
// RawURLEncoding, and Crockford's base32 without padding, for backup codes.

const std = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
const url = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_';
/** @internal Crockford's base32: digits and capitals without I, L, O and U. */
export const crockford = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';

function encode64(b: Uint8Array, alphabet: string): string {
  let s = '';
  for (let i = 0; i < b.length; i += 3) {
    const n = (b[i] << 16) | ((b[i + 1] ?? 0) << 8) | (b[i + 2] ?? 0);
    const chars = Math.min(4, Math.ceil(((b.length - i) * 8) / 6));
    for (let j = 0; j < chars; j++) s += alphabet[(n >> (18 - 6 * j)) & 63];
  }
  return s;
}

/** @internal */
export function base64(b: Uint8Array): string {
  return encode64(b, std);
}

/** @internal */
export function base64URL(b: Uint8Array): string {
  return encode64(b, url);
}

/** @internal Decodes standard base64 without padding; throws on anything else. */
export function unbase64(s: string): Uint8Array {
  if (s.length % 4 === 1) throw new Error('base64: bad length');
  const out = new Uint8Array(Math.floor((s.length * 6) / 8));
  let acc = 0, bits = 0, o = 0;
  for (let i = 0; i < s.length; i++) {
    const v = std.indexOf(s[i]);
    if (v < 0) throw new Error(`base64: illegal character at ${i}`);
    acc = (acc << 6) | v;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out[o++] = (acc >> bits) & 0xff;
    }
    acc &= (1 << bits) - 1;
  }
  return out;
}

/** @internal */
export function base32Crockford(b: Uint8Array): string {
  let s = '', acc = 0, bits = 0;
  for (const byte of b) {
    acc = (acc << 8) | byte;
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      s += crockford[(acc >> bits) & 31];
    }
    acc &= (1 << bits) - 1;
  }
  if (bits > 0) s += crockford[(acc << (5 - bits)) & 31];
  return s;
}

const encoder = new TextEncoder();
const decoder = new TextDecoder();

/** @internal */
export const utf8 = (s: string): Uint8Array => encoder.encode(s);
/** @internal */
export const unutf8 = (b: Uint8Array): string => decoder.decode(b);

/** @internal Cryptographically random bytes. */
export function random(n: number): Uint8Array {
  return globalThis.crypto.getRandomValues(new Uint8Array(n));
}
