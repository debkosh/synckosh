// What a server answers, the same for both ends.

import { fromWire, type Item, type WireItem } from './item.js';

/**
 * Result is the server's answer for one pushed item: stored with its sequence number, or not
 * stored because the server holds a version with a later timestamp, returned in `current`, or
 * refused with a `code` (one of Refused...) and an `error` to read.
 */
export interface Result {
  ok: boolean;
  sequence?: number;
  current?: Item;
  code?: string;
  error?: string;
}

/** Pushed is the server's answer to a push: one result per item, in the order sent. */
export interface Pushed {
  store: string;
  /** The server's clock, in milliseconds. */
  time: number;
  results: Result[];
}

/**
 * Page is one page of a pull. `time` is the server's clock, and `cleanup` the age after which it
 * removes deleted items, both in milliseconds; `cleanup` is 0 if it never does.
 */
export interface Page {
  store: string;
  items: Item[];
  next: number;
  more: boolean;
  time: number;
  cleanup: number;
}

/** The item's timestamp is more than MaxAhead ahead of the server's clock: the device's clock is wrong. */
export const RefusedTimestampAhead = 'timestamp-ahead';
/** The item is over the server's item limit. */
export const RefusedTooBig = 'too-big';
/** A field is malformed, such as a collection name with capitals. */
export const RefusedInvalid = 'invalid';
/**
 * The item is new to the server, and a live item of its collection has its identity. The result's
 * `current` is that item, which the device checks and takes.
 */
export const RefusedIdentityTaken = 'identity-taken';

/**
 * Limits bound what one request carries. The server and each device set their own; a missing or
 * zero field means the default in DefaultLimits.
 */
export interface Limits {
  /** One item as JSON on the wire, sealed. */
  itemBytes?: number;
  /** Items in one push. */
  pushItems?: number;
  /** One push request's body. */
  pushBytes?: number;
  /** The items of one pull page, but always at least one item. */
  pageBytes?: number;
}

/** DefaultLimits are the limits a side uses unless it sets its own. */
export const DefaultLimits: Readonly<Required<Limits>> = Object.freeze({
  itemBytes: 1 << 20, pushItems: 1000, pushBytes: 16 << 20, pageBytes: 16 << 20,
});

/** orDefaults returns the limits with every missing or zero field set to its default. */
export function orDefaults(l: Limits = {}): Required<Limits> {
  return {
    itemBytes: l.itemBytes || DefaultLimits.itemBytes,
    pushItems: l.pushItems || DefaultLimits.pushItems,
    pushBytes: l.pushBytes || DefaultLimits.pushBytes,
    pageBytes: l.pageBytes || DefaultLimits.pageBytes,
  };
}

/**
 * The one name the protocol keeps for itself: the collection of the library's own items. It holds
 * the data key's sealed copies, of datatype SealedKeyDatatype, and each collection's
 * delete-before, of datatype DeleteBeforeDatatype, and nothing else. An app's collections are its
 * own, and so is every datatype outside this collection.
 */
export const LibraryCollection = '_synckosh';
/** The datatype of a sealed copy of the data key. */
export const SealedKeyDatatype = 'sealedkey';
/** The datatype of a collection's delete-before. */
export const DeleteBeforeDatatype = 'deletebefore';

/** deleteBeforeGUID is the guid of a collection's delete-before, the same on every device, so a collection has one. */
export function deleteBeforeGUID(collection: string): string {
  return 'deletebefore:' + collection;
}

const nameRe = /^[a-z0-9][a-z0-9._-]{0,63}$/;
const guidRe = /^[A-Za-z0-9._:~-]{1,128}$/;
const identityRe = /^[0-9a-f]{64}$/;

/**
 * valid checks an item's names and timestamp, and for the library's own items what the protocol
 * reads in them. It throws for an invalid one. The server refuses an invalid item, a device won't
 * put one, and a device takes none from the server.
 */
export function valid(it: Item): void {
  const lib = it.collection === LibraryCollection;
  if (!lib && !nameRe.test(it.collection)) {
    throw new Error('collection: lowercase letters, digits, dot, dash or underscore, up to 64, starting with a letter or digit');
  } else if (!nameRe.test(it.datatype)) {
    throw new Error('datatype: lowercase letters, digits, dot, dash or underscore, up to 64, starting with a letter or digit');
  } else if (!guidRe.test(it.guid)) {
    throw new Error('guid: letters, digits and ._:~- up to 128');
  } else if (!(it.timestamp > 0)) {
    throw new Error('timestamp: the time of the edit, in milliseconds');
  } else if (it.identity && !identityRe.test(it.identity)) {
    throw new Error('identity: 64 lowercase hex digits, a SHA-256');
  }
  if (!lib || it.datatype === SealedKeyDatatype) return;
  if (it.datatype !== DeleteBeforeDatatype) {
    throw new Error(`the ${LibraryCollection} collection holds only ${SealedKeyDatatype} and ${DeleteBeforeDatatype} items`);
  }
  const { collection } = cutoff(it);
  if (it.guid !== deleteBeforeGUID(collection)) throw new Error("a delete-before's guid is deleteBeforeGUID of its collection");
  if (it.deleted) throw new Error('a delete-before is never deleted');
}

/** isDeleteBefore says whether an item is a delete-before. */
export function isDeleteBefore(it: Item): boolean {
  return it.collection === LibraryCollection && it.datatype === DeleteBeforeDatatype;
}

/**
 * cutoff reads a delete-before: the collection it deletes from, and its cutoff, from its public
 * part `{"collection": name, "before": ms}`.
 */
export function cutoff(it: Item): { collection: string; before: number } {
  if (!isDeleteBefore(it)) throw new Error('not a delete-before');
  const p = it.public as { collection?: unknown; before?: unknown } | undefined;
  const ok = typeof p === 'object' && p !== null && !Array.isArray(p);
  const collection = ok ? p.collection : undefined, before = ok ? p.before : undefined;
  if (typeof collection !== 'string' || !nameRe.test(collection) || !Number.isSafeInteger(before) || (before as number) <= 0) {
    throw new Error(`a delete-before's public part is {"collection": an app's collection, "before": milliseconds}`);
  }
  return { collection, before: before as number };
}

/** Filter limits a pull to some collections and some datatypes. A missing or empty list allows all: `{}` is every item. */
export interface Filter {
  collections?: string[];
  datatypes?: string[];
}

/** validName checks a collection name, as a pull asks for one. */
export function validName(collection: string): boolean {
  return collection === LibraryCollection || nameRe.test(collection);
}

/** @internal Reads a push answer. */
export function readPushed(v: any): Pushed {
  if (typeof v !== 'object' || v === null || !Array.isArray(v.results)) throw new Error('push: not an answer');
  return {
    store: String(v.store ?? ''),
    time: Number(v.time ?? 0),
    results: v.results.map((r: any): Result => {
      const out: Result = { ok: r?.ok === true };
      if (r?.sequence) out.sequence = r.sequence;
      if (r?.current) out.current = fromWire(r.current as WireItem);
      if (r?.code) out.code = String(r.code);
      if (r?.error) out.error = String(r.error);
      return out;
    }),
  };
}

/** @internal Reads a pull page. */
export function readPage(v: any): Page {
  if (typeof v !== 'object' || v === null) throw new Error('pull: not an answer');
  return {
    store: String(v.store ?? ''),
    items: (v.items ?? []).map(fromWire),
    next: Number(v.next ?? 0),
    more: v.more === true,
    time: Number(v.time ?? 0),
    cleanup: Number(v.cleanup ?? 0),
  };
}

/** @internal Reads limits from the wire. */
export function readLimits(v: any): Limits {
  return { itemBytes: v?.item_bytes, pushItems: v?.push_items, pushBytes: v?.push_bytes, pageBytes: v?.page_bytes };
}
