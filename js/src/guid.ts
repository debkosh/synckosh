import { random } from './encoding.js';

let lastMs = 0;
let count = 0;

const hex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');

/**
 * newGUID returns a new item's guid: a UUIDv7 (RFC 9562), which sorts by when it was made.
 * Guids made in the same millisecond still sort in the order they were made: the 12 bits after
 * the time count up, as RFC 9562 section 6.2 method 1 allows.
 */
export function newGUID(): string {
  let ms = Date.now();
  if (ms <= lastMs) {
    count++;
    if (count > 0xfff) {
      lastMs++;
      count = 0;
    }
    ms = lastMs;
  } else {
    lastMs = ms;
    count = 0;
  }
  const u = new Uint8Array(16);
  u.set(random(10), 6);
  for (let i = 5, t = ms; i >= 0; i--, t = Math.floor(t / 256)) u[i] = t % 256;
  u[6] = 0x70 | (count >> 8); // version 7
  u[7] = count & 0xff;
  u[8] = 0x80 | (u[8] & 0x3f); // variant 10
  const h = hex(u);
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}
