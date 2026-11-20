// The item, and how it goes on the wire.

/**
 * Item is one synced thing. `public` is plain: the server and a device that hasn't unlocked can
 * read it. `private` is plain on the device and sealed on the wire. Both are any JSON value, or
 * undefined for none. `base` is the sequence number of the version an edit was made from,
 * absent or 0 for none. `sequence` and `deletedAt` are set by the server. A field the item
 * doesn't have is dropped when it is read.
 */
export interface Item {
  collection: string;
  datatype: string;
  guid: string;
  /** When the edit was made: the device's clock, in milliseconds. */
  timestamp: number;
  deleted: boolean;
  public?: unknown;
  private?: unknown;
  base?: number;
  sequence?: number;
  /** The server's clock, in milliseconds, when it stored the delete. */
  deletedAt?: number;
}

/** The JSON object of an item as the wire carries it. */
export type WireItem = Record<string, unknown>;

/**
 * toWire is an item as the wire carries it, with its keys in order, so one item is always the
 * same JSON.
 */
export function toWire(it: Item): WireItem {
  const m: WireItem = {};
  m.collection = it.collection;
  m.datatype = it.datatype;
  m.guid = it.guid;
  m.timestamp = it.timestamp;
  m.deleted = it.deleted;
  if (it.public !== undefined && it.public !== null) m.public = it.public;
  if (it.private !== undefined && it.private !== null) m.private = it.private;
  if (it.base) m.base = it.base;
  if (it.sequence) m.sequence = it.sequence;
  if (it.deletedAt) m.deleted_at = it.deletedAt;
  const sorted: WireItem = {};
  for (const k of Object.keys(m).sort()) sorted[k] = m[k];
  return sorted;
}

function field<T>(m: WireItem, k: string, ok: (v: unknown) => boolean, zero: T): T {
  if (!(k in m) || m[k] === null) return zero;
  if (!ok(m[k])) throw new Error(`item ${k}: ${JSON.stringify(m[k])}`);
  return m[k] as T;
}

const isString = (v: unknown) => typeof v === 'string';
const isInt = (v: unknown) => Number.isSafeInteger(v);
const isUint = (v: unknown) => Number.isSafeInteger(v) && (v as number) >= 0;

/** fromWire reads an item from the wire. It throws for a field of the wrong type. */
export function fromWire(w: unknown): Item {
  if (typeof w !== 'object' || w === null || Array.isArray(w)) throw new Error('item: not a JSON object');
  const m = w as WireItem;
  const it: Item = {
    collection: field(m, 'collection', isString, ''),
    datatype: field(m, 'datatype', isString, ''),
    guid: field(m, 'guid', isString, ''),
    timestamp: field(m, 'timestamp', isInt, 0),
    deleted: field(m, 'deleted', (v) => typeof v === 'boolean', false),
  };
  if (m.public !== undefined && m.public !== null) it.public = m.public;
  if (m.private !== undefined && m.private !== null) it.private = m.private;
  const base = field(m, 'base', isUint, 0), sequence = field(m, 'sequence', isUint, 0);
  const deletedAt = field(m, 'deleted_at', isInt, 0);
  if (base) it.base = base;
  if (sequence) it.sequence = sequence;
  if (deletedAt) it.deletedAt = deletedAt;
  return it;
}

/** @internal JSON of a part, or null when there is none. */
export function json(v: unknown): string {
  return v === undefined ? 'null' : JSON.stringify(v);
}

/** @internal A deep copy of an item, so a caller's item and a stored one never share. */
export function copyItem(it: Item): Item {
  return structuredClone(it);
}
