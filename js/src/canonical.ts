// Canonical JSON and items' identities.

import { hmac } from '@noble/hashes/hmac.js';
import { sha256 } from '@noble/hashes/sha2.js';
import { utf8 } from './encoding.js';
import type { Item } from './item.js';
import type { DataKey } from './keys.js';

/**
 * canonical writes a JSON value in one form only, so equal values are equal bytes in any
 * language: RFC 8785's, which is JSON.stringify with object keys in order of their UTF-16 code
 * units.
 */
export function canonical(v: unknown): string {
  if (v === null || typeof v !== 'object') {
    if (typeof v === 'number' && !Number.isFinite(v)) throw new Error(`canonical: number ${v}`);
    return JSON.stringify(v ?? null);
  }
  if (Array.isArray(v)) return '[' + v.map((e) => canonical(e === undefined ? null : e)).join(',') + ']';
  const o = v as Record<string, unknown>;
  return '{' + Object.keys(o).filter((k) => o[k] !== undefined).sort()
    .map((k) => JSON.stringify(k) + ':' + canonical(o[k])).join(',') + '}';
}

/**
 * sameContent is the identity an item has unless its app says otherwise: its public and private
 * parts, in canonical JSON, as the array [public, private], null for a part it lacks. Two items
 * that hold the same are one thing.
 */
export function sameContent(it: Pick<Item, 'public' | 'private'>): Uint8Array {
  return utf8(canonical([it.public ?? null, it.private ?? null]));
}

const hex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');

/**
 * identity hashes what makes two items one thing into an item's identity, for the server's index:
 * HMAC-SHA256 under a key made from the data key, so the server learns only that two items match,
 * never what they are. A bucket without a key is plain to the server, and there it is SHA-256.
 */
export function identity(key: DataKey | undefined, what: Uint8Array): string {
  if (!key) return hex(sha256(what));
  return hex(hmac(sha256, hmac(sha256, key.bytes(), utf8('synckosh identity')), what));
}
