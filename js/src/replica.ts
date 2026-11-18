import { Clock, MaxAhead } from './clock.js';
import { Client, TooLargeError } from './client.js';
import { newGUID } from './guid.js';
import { copyItem, json, toWire, type Item } from './item.js';
import { DataKey, PassphraseError, UnknownKeyError, openSealedCopy } from './keys.js';
import type { Entry, Local } from './local.js';
import {
  cutoff as cutoffOf, DeleteBeforeItem, KeysCollection, orDefaults, RefusedTimestampAhead, valid,
  type Limits, type Page,
} from './wire.js';

/**
 * Merger joins two versions of one item that never saw each other: one edited here, or held
 * here, and one from the server. It gets no common ancestor, so it is only right for data that
 * can be joined without one: fields that carry their own clocks, or sets that only grow. It must
 * be a pure function, and should give the same content whichever version is local. Only
 * `deleted`, `public` and `private` of the result are used.
 */
export type Merger = (local: Item, remote: Item) => Pick<Item, 'deleted' | 'public' | 'private'>;

/** LockedError: the item is sealed under a key this device doesn't hold, so it can't be read or changed here. */
export class LockedError extends Error {
  constructor(what: string, readonly item?: Item) {
    super(`${what}: sealed with a key this device doesn't have`);
    this.name = 'LockedError';
  }
}

/** HasKeyError is create's answer for a bucket that already holds items: join it with a passphrase. */
export class HasKeyError extends Error {
  constructor() {
    super('the bucket already holds items: join it with a passphrase');
    this.name = 'HasKeyError';
  }
}

/** Refused is a pushed item the server refused. It stays pending, and goes out again on the next sync. */
export interface Refused {
  collection: string;
  guid: string;
  /** One of Refused... */
  code: string;
  error: string;
}

/**
 * RefusedError is what sync throws when the server refused some items. Everything else was
 * synced. When the code is RefusedTimestampAhead, this device's clock is wrong, by `aheadMs`.
 */
export class RefusedError extends Error {
  constructor(readonly items: Refused[], readonly aheadMs: number) {
    const ahead = items.some((it) => it.code === RefusedTimestampAhead);
    super(ahead
      ? `sync: the server refused ${items.length} items: this device's clock is ${Math.round(aheadMs / 60000)} minutes ahead of the server's`
      : `sync: the server refused ${items.length} items: ${items[0].collection} ${items[0].guid}: ${items[0].error}`);
    this.name = 'RefusedError';
  }
}

/** Rejected is an item from the server that this device didn't take: invalid, not sealed, or changed after it was sealed. */
export interface Rejected {
  collection: string;
  guid: string;
  error: string;
}

/** RejectedError is what sync throws when it rejected items from the server. Everything else was synced. */
export class RejectedError extends Error {
  constructor(readonly items: Rejected[]) {
    super(`sync: rejected ${items.length} items from the server: ${items[0].collection} ${items[0].guid}: ${items[0].error}`);
    this.name = 'RejectedError';
  }
}

/** What a Replica needs. It syncs every collection: one the app or another device starts using needs no setup. */
export interface Config {
  client: Client;
  local: Local;
  /** Seals every item, and every item from the server must be sealed with it. Without a key everything is plain, and the server is trusted. */
  key?: DataKey;
  /** The datatypes whose public part is sealed too, with a copy in the box, so the server can't change it. */
  sealPublic?: string[];
  /**
   * Keeps deleted items here, with their data, until the app calls cleanup: items() hides them,
   * get() reports them deleted, and putting one again restores it.
   */
  keepDeleted?: boolean;
  /** Join concurrent versions, by datatype. A datatype with none keeps the newest. */
  mergers?: Record<string, Merger>;
  /** This device's limits; missing fields take DefaultLimits. */
  limits?: Limits;
}

/** An item to put: a new one needs no guid, and the replica sets its timestamp and base. */
export type PutItem = Pick<Item, 'collection' | 'datatype'> & Partial<Item>;

/** Bounds the pull and push rounds of one sync. A round repeats only when the server changed its store id. */
const maxRounds = 5;

/** What one round of a sync couldn't sync. */
class Report {
  refused: Refused[] = [];
  ahead = 0;
  rejected: Rejected[] = [];
  reject(it: Item, err: unknown) {
    this.rejected.push({ collection: it.collection, guid: it.guid, error: (err as Error).message ?? String(err) });
  }
  error(): Error | undefined {
    const errs: Error[] = [];
    if (this.refused.length) errs.push(new RefusedError(this.refused, this.ahead));
    if (this.rejected.length) errs.push(new RejectedError(this.rejected));
    if (errs.length <= 1) return errs[0];
    return new AggregateError(errs, errs.map((e) => e.message).join('\n'));
  }
}

/** What one page, or one push's answer, does to this device's entries, stored together. */
class Changes {
  put: Entry[] = [];
  gone = new Map<string, string[]>();
  remove(it: Item) {
    const g = this.gone.get(it.collection);
    if (g) g.push(it.guid);
    else this.gone.set(it.collection, [it.guid]);
  }
}

const what = (collection: string, guid: string) => `${collection} ${guid}`;
const copyEntry = (e: Entry): Entry => ({ ...e, item: copyItem(e.item) });

/** Replica is one device's copy of a bucket. Its methods run one at a time, in the order called. */
export class Replica {
  readonly #c: Client;
  readonly #local: Local;
  readonly #key?: DataKey;
  readonly #sealPublic: Set<string>;
  readonly #keepDeleted: boolean;
  readonly #mergers: Record<string, Merger>;
  #limits: Required<Limits>;
  readonly #clock = new Clock();
  #queue: Promise<unknown> = Promise.resolve();

  private constructor(cfg: Config) {
    this.#c = cfg.client;
    this.#local = cfg.local;
    this.#key = cfg.key;
    this.#sealPublic = new Set(cfg.sealPublic ?? []);
    this.#keepDeleted = cfg.keepDeleted ?? false;
    this.#mergers = cfg.mergers ?? {};
    this.#limits = orDefaults(cfg.limits);
  }

  /** open opens a device's replica, with its clock where it left off. */
  static async open(cfg: Config): Promise<Replica> {
    const r = new Replica(cfg);
    const b = await r.#local.meta('clock');
    if (b !== undefined) {
      const last = JSON.parse(b);
      if (!Number.isSafeInteger(last)) throw new Error(`clock: ${b}`);
      r.#clock.observe(last);
    }
    return r;
  }

  #run<T>(fn: () => Promise<T>): Promise<T> {
    const p = this.#queue.then(fn);
    this.#queue = p.catch(() => {});
    return p;
  }

  async #saveClock() {
    await this.#local.setMeta('clock', JSON.stringify(this.#clock.last()));
  }

  /** An item as it goes to the server. */
  #seal(it: Item): Item {
    return this.#key ? this.#key.seal(it, this.#sealPublic.has(it.datatype)) : it;
  }

  /** An item from the server as this device keeps it: throws UnknownKeyError for another key's, and otherwise for one changed or forged. */
  #open(it: Item): Item {
    return this.#key ? this.#key.open(it) : it;
  }

  /**
   * put records edits made here, in one write. A new item gets a new guid; an existing one keeps
   * its guid. Timestamps are set here. It returns the items as stored. If any item is invalid,
   * too big to push once sealed, or locked, it throws and stores none of them.
   */
  put(...items: PutItem[]): Promise<Item[]> {
    return this.#run(async () => {
      const out: Item[] = [];
      for (const given of items) {
        const it = copyItem({ ...given, guid: given.guid ?? '', timestamp: 0, deleted: false });
        if (it.datatype === DeleteBeforeItem) throw new Error('put: a delete-before is written with deleteBefore');
        if (!it.guid) it.guid = newGUID();
        const old = await this.#local.get(it.collection, it.guid);
        if (old?.locked) throw new LockedError(what(it.collection, it.guid));
        if (old) {
          if (old.item.extra) it.extra = old.item.extra;
          else delete it.extra;
        }
        it.deleted = false;
        delete it.sequence;
        delete it.deletedAt;
        out.push(it);
      }
      await this.#write(out);
      return out.map(copyItem);
    });
  }

  /**
   * delete marks an item deleted. The server keeps it, with its data, until its cleanup. This
   * device removes it once the server holds the delete, or with keepDeleted keeps it, so putting
   * it again restores it.
   */
  delete(collection: string, guid: string): Promise<void> {
    return this.#run(async () => {
      const e = await this.#local.get(collection, guid);
      if (!e || e.item.deleted) return;
      if (e.locked) throw new LockedError(what(collection, guid));
      const it = e.item;
      it.deleted = true;
      delete it.sequence;
      delete it.deletedAt;
      await this.#write([it]);
    });
  }

  /**
   * deleteBefore deletes every item of a collection with a timestamp at or before `before`, on
   * every device, in one item: the collection's delete-before. The server forgets those items at
   * once, and stores none at or before the cutoff again. The cutoff only moves forward.
   */
  deleteBefore(collection: string, before: Date | number): Promise<void> {
    return this.#run(async () => {
      let cut = typeof before === 'number' ? before : before.getTime();
      if (cut > Date.now() + MaxAhead) throw new Error(`delete before ${new Date(cut).toISOString()}: more than an hour ahead`);
      const old = await this.#local.get(collection, DeleteBeforeItem);
      if (old?.locked) throw new LockedError(what(collection, DeleteBeforeItem));
      const it: Item = { collection, datatype: DeleteBeforeItem, guid: DeleteBeforeItem, timestamp: 0, deleted: false };
      if (old) {
        let held = 0;
        try {
          held = cutoffOf(old.item);
        } catch {}
        cut = Math.max(cut, held);
        if (old.item.extra) it.extra = old.item.extra;
      }
      it.public = { before: cut };
      await this.#write([it]);
      await this.#cut(it);
    });
  }

  /** Follows a delete-before this device wrote or took. */
  async #cut(it: Item) {
    const cut = cutoffOf(it);
    this.#clock.observe(cut);
    if (this.#keepDeleted) return;
    const gone = (await this.#local.entries(it.collection))
      .filter((e) => e.item.guid !== DeleteBeforeItem && e.item.timestamp <= cut)
      .map((e) => e.item.guid);
    await this.#local.remove(it.collection, gone);
  }

  /** A collection's delete-before cutoff, 0 if it has none this device can open. */
  async #cutoff(collection: string): Promise<number> {
    const e = await this.#local.get(collection, DeleteBeforeItem);
    if (!e || e.locked) return 0;
    return cutoffOf(e.item);
  }

  /**
   * Stores edits made here, all with one timestamp. Each edit's base is the sequence number of
   * the stored version it was made from; an edit of an edit not yet sent keeps that one's base,
   * and its sent, as the server may hold it already.
   */
  async #write(items: Item[]) {
    const ts = this.#clock.now();
    const es: Entry[] = [];
    for (const it of items) {
      const e: Entry = { item: it, pending: true };
      delete it.base;
      const old = await this.#local.get(it.collection, it.guid);
      if (old?.pending) {
        if (old.item.base) it.base = old.item.base;
        if (old.sent) e.sent = old.sent;
      } else if (old?.item.sequence) {
        it.base = old.item.sequence;
      }
      it.timestamp = ts;
      this.#check(it);
      es.push(e);
    }
    await this.#local.put(es);
    await this.#saveClock();
  }

  /** Refuses an edit the server would refuse for good: an invalid name, or one over the item limit once sealed. */
  #check(it: Item) {
    try {
      valid(it);
    } catch (e) {
      throw new Error(`${what(it.collection, it.guid)}: ${(e as Error).message}`);
    }
    const n = new TextEncoder().encode(JSON.stringify(toWire(this.#seal(it)))).length;
    if (n > this.#limits.itemBytes) {
      throw new Error(`${what(it.collection, it.guid)}: ${n} bytes sealed, over the item limit of ${this.#limits.itemBytes}`);
    }
  }

  /**
   * get returns an item, deleted or not, or undefined if this device has none. It throws a
   * LockedError, holding the item as it came, for one this device can't open. An item at or
   * before its collection's delete-before cutoff is reported deleted.
   */
  get(collection: string, guid: string): Promise<Item | undefined> {
    return this.#run(async () => {
      const e = await this.#local.get(collection, guid);
      if (!e) return undefined;
      if (e.locked) throw new LockedError(what(collection, guid), e.item);
      const cut = await this.#cutoff(collection);
      if (guid !== DeleteBeforeItem && e.item.timestamp <= cut) e.item.deleted = true;
      return e.item;
    });
  }

  /**
   * items returns a collection's live items, in guid order, which is the order they were made.
   * Deleted and locked items are left out, and so are items at or before the collection's
   * delete-before cutoff.
   */
  items(collection: string): Promise<Item[]> {
    return this.#run(async () => {
      const cut = await this.#cutoff(collection);
      return (await this.#local.entries(collection))
        .filter((e) => !e.locked && !e.item.deleted && e.item.guid !== DeleteBeforeItem && e.item.timestamp > cut)
        .map((e) => e.item);
    });
  }

  /**
   * locked returns a collection's items sealed under a key this device doesn't hold, as they
   * came. Nothing in them is verified, so an app must not trust what they show.
   */
  locked(collection: string): Promise<Item[]> {
    return this.#run(async () => (await this.#local.entries(collection)).filter((e) => e.locked).map((e) => e.item));
  }

  /**
   * cleanup removes this device's entries deleted longer ago than ageMs: by when the server
   * stored the delete if this device knows it, else by the delete's timestamp; for items at or
   * before a delete-before cutoff, by when the delete-before was written. Pending entries stay.
   * It returns how many it removed.
   */
  cleanup(ageMs: number): Promise<number> {
    return this.#run(async () => {
      const before = Date.now() - ageMs;
      const all = await this.#local.entries('');
      const cuts = new Map<string, number>();
      for (const e of all) {
        if (!e.locked && e.item.guid === DeleteBeforeItem && e.item.timestamp < before) {
          let c = 0;
          try {
            c = cutoffOf(e.item);
          } catch {}
          cuts.set(e.item.collection, c);
        }
      }
      const gone = new Changes();
      let n = 0;
      for (const e of all) {
        const it = e.item;
        const deletedAt = it.deletedAt || it.timestamp;
        if (!e.pending && !e.locked && it.guid !== DeleteBeforeItem &&
          ((it.deleted && deletedAt < before) || it.timestamp <= (cuts.get(it.collection) ?? 0))) {
          gone.remove(it);
          n++;
        }
      }
      for (const [collection, guids] of gone.gone) await this.#local.remove(collection, guids);
      return n;
    });
  }

  /**
   * sync pulls what is new, then pushes what changed here. If the server's store id has changed
   * since this device last synced, the server may have lost data, so the device pushes all it
   * has. If the server refused some items, sync syncs the rest and throws a RefusedError; if it
   * rejected items from the server, a RejectedError; when both, an AggregateError of the two.
   */
  sync(signal?: AbortSignal): Promise<void> {
    return this.#run(async () => {
      for (let round = 0; round < maxRounds; round++) {
        const rp = new Report();
        const again = await this.#pull(rp, signal);
        const restart = await this.#push(rp, signal);
        await this.#saveClock();
        if (!again && !restart) {
          const err = rp.error();
          if (err) throw err;
          return;
        }
      }
      throw new Error(`sync: not settled after ${maxRounds} rounds`);
    });
  }

  /**
   * Compares the server's store id with the one this device last synced with. On a new one,
   * the device refills the server. The store id is saved last: a crash before repeats the refill.
   */
  async #checkStore(id: string): Promise<boolean> {
    const old = await this.#local.meta('store');
    if (old === id) return false;
    if (old !== undefined) await this.#refill();
    await this.#local.setMeta('store', id);
    return old !== undefined;
  }

  /** Marks every entry this device can open pending, to push it all back, and reads from the start. */
  async #refill() {
    const all = (await this.#local.entries('')).map((e) => {
      e.pending = e.pending || !e.locked;
      delete e.item.sequence;
      delete e.item.base;
      delete e.sent;
      return e;
    });
    await this.#local.put(all);
    await this.#setCursor(0);
    await this.#local.setMeta('pulled', undefined);
  }

  /** The cursor is the sequence number of the last item this device has from the server. */
  async #cursor(): Promise<number> {
    const b = await this.#local.meta('after');
    return b === undefined ? 0 : JSON.parse(b);
  }

  async #setCursor(n: number) {
    await this.#local.setMeta('after', JSON.stringify(n));
  }

  /** Stores an entry, or removes it if it is deleted and this device doesn't keep deletes. */
  #keep(ch: Changes, e: Entry) {
    if (e.item.deleted && !e.pending && !e.locked && !this.#keepDeleted) ch.remove(e.item);
    else ch.put.push(e);
  }

  async #apply(ch: Changes) {
    await this.#local.put(ch.put);
    for (const [collection, guids] of ch.gone) await this.#local.remove(collection, guids);
    for (const e of ch.put) {
      if (!e.locked && e.item.guid === DeleteBeforeItem) await this.#cut(e.item);
    }
  }

  async #pull(rp: Report, signal?: AbortSignal): Promise<boolean> {
    let restart = false;
    let after = await this.#cursor();
    for (;;) {
      const p = await this.#c.pull('', after, this.#limits.pageBytes, signal);
      if (await this.#checkStore(p.store)) {
        restart = true;
        after = 0;
        continue;
      }
      if (await this.#stale(p)) {
        await this.#startOver();
        after = 0;
        continue;
      }
      const ch = new Changes();
      for (const it of p.items) await this.#take(it, rp, ch);
      await this.#apply(ch);
      after = p.next;
      await this.#setCursor(after);
      if (!p.more) {
        await this.#local.setMeta('pulled', JSON.stringify(p.time));
        return restart;
      }
    }
  }

  /** Whether this device last finished a pull longer ago than the server keeps deleted items. */
  async #stale(p: Page): Promise<boolean> {
    const b = await this.#local.meta('pulled');
    if (b === undefined || p.cleanup <= 0) return false;
    const pulled = JSON.parse(b);
    if (!Number.isSafeInteger(pulled)) throw new Error(`pulled: ${b}`);
    return p.time - pulled > p.cleanup;
  }

  /** Makes a stale device a new one, but for its edits not yet sent. */
  async #startOver() {
    const gone = new Changes();
    for (const e of await this.#local.entries('')) if (!e.pending) gone.remove(e.item);
    for (const [collection, guids] of gone.gone) await this.#local.remove(collection, guids);
    await this.#setCursor(0);
    await this.#local.setMeta('pulled', undefined);
  }

  /**
   * Decides what an item from the server does here: it is taken, unless this device has a
   * version with a later timestamp. One sealed under a key this device doesn't hold is locked:
   * kept only where this device holds nothing it can open, or an older locked version. One that
   * is invalid, not sealed, or changed after it was sealed is rejected.
   */
  async #take(remote: Item, rp: Report, ch: Changes) {
    try {
      valid(remote);
    } catch (e) {
      rp.reject(remote, e);
      return;
    }
    let opened: Item | undefined;
    let locked = false;
    try {
      opened = this.#open(remote);
      valid(opened); // a delete-before's cutoff, from the box
    } catch (e) {
      if (e instanceof UnknownKeyError) locked = true;
      else {
        rp.reject(remote, e);
        return;
      }
    }
    const e = await this.#local.get(remote.collection, remote.guid);
    if (locked) {
      if (!e || (e.locked && e.item.timestamp <= remote.timestamp)) ch.put.push({ item: remote, locked: true });
      return;
    }
    const got = opened!;
    this.#clock.observe(got.timestamp);
    if (!e || e.locked) {
      this.#keep(ch, { item: got });
      return;
    }
    if (e.sent && got.timestamp === e.sent) {
      // This device's own push, whose answer was lost: they have met.
      if (e.item.timestamp === e.sent) {
        this.#keep(ch, { item: got });
      } else {
        // edited again since
        if (got.sequence) e.item.base = got.sequence;
        else delete e.item.base;
        delete e.sent;
        ch.put.push(e);
      }
      return;
    }
    const merge = this.#mergers[got.datatype];
    if (merge && neverMet(e, got) && e.item.datatype === got.datatype) {
      const m = merge(copyItem(e.item), copyItem(got));
      if (sameContent(e.item, got) || sameContent(m, got)) {
        this.#keep(ch, { item: got });
        return;
      }
      // A new edit, after the remote one and made from it.
      const it: Item = { ...got, deleted: m.deleted };
      delete it.public;
      delete it.private;
      if (m.public !== undefined && m.public !== null) it.public = structuredClone(m.public);
      if (m.private !== undefined && m.private !== null) it.private = structuredClone(m.private);
      it.timestamp = this.#clock.now();
      if (got.sequence) it.base = got.sequence;
      else delete it.base;
      delete it.sequence;
      delete it.deletedAt;
      ch.put.push({ item: it, pending: true });
      return;
    }
    if (e.item.timestamp > got.timestamp) return; // this device's version is later, and is pushed
    this.#keep(ch, { item: got });
  }

  /**
   * Sends what changed here, as many items at a time as the limits allow. When a push's
   * sequence numbers follow straight on from the cursor, nothing else reached the server in
   * between, so the cursor moves past them and this device doesn't pull its own items back.
   */
  async #push(rp: Report, signal?: AbortSignal): Promise<boolean> {
    let pending = (await this.#local.pending()).sort((a, b) => a.item.timestamp - b.item.timestamp);
    let cur = await this.#cursor();
    const enc = new TextEncoder();
    while (pending.length > 0) {
      const out: Item[] = [];
      let size = '{"items":[]}'.length;
      while (out.length < pending.length && out.length < this.#limits.pushItems) {
        const sealed = this.#seal(pending[out.length].item);
        const n = enc.encode(JSON.stringify(toWire(sealed))).length;
        if (out.length > 0 && size + n + 1 > this.#limits.pushBytes) break;
        size += n + 1;
        out.push(sealed);
      }
      const batch = pending.slice(0, out.length);
      for (const e of batch) e.sent = e.item.timestamp;
      await this.#local.put(batch);
      let res;
      try {
        res = await this.#c.push(out, signal);
      } catch (err) {
        if (err instanceof TooLargeError && this.#fit(err.limits)) continue; // split to fit, and carry on
        throw err;
      }
      pending = pending.slice(out.length);
      if (await this.#checkStore(res.store)) return true;
      const ch = new Changes();
      let lo = 0, hi = 0;
      for (const [i, rs] of res.results.entries()) {
        if (rs.ok) {
          // Stored, or, with no sequence number, at or before the collection's delete-before
          // cutoff, which the server doesn't store again.
          const e = copyEntry(batch[i]);
          if (rs.sequence) e.item.sequence = rs.sequence;
          else delete e.item.sequence;
          delete e.pending;
          delete e.sent;
          this.#keep(ch, e);
          if (!rs.sequence) continue;
          if (lo === 0 || rs.sequence < lo) lo = rs.sequence;
          hi = Math.max(hi, rs.sequence);
        } else if (rs.current) {
          // The server holds a later version: it wins, here too.
          await this.#take(rs.current, rp, ch);
        } else {
          const it = batch[i].item;
          rp.refused.push({ collection: it.collection, guid: it.guid, code: rs.code ?? '', error: rs.error ?? '' });
          if (rs.code === RefusedTimestampAhead) {
            // Never stored anywhere, so it can take a new timestamp.
            rp.ahead = Math.max(rp.ahead, it.timestamp - res.time);
            this.#clock.reset(res.time);
            const e = copyEntry(batch[i]);
            e.item.timestamp = this.#clock.now();
            delete e.sent;
            ch.put.push(e);
          }
        }
      }
      await this.#apply(ch);
      if (lo === cur + 1) {
        cur = hi;
        await this.#setCursor(cur);
      }
    }
    return false;
  }

  /** Lowers this device's push limits to the server's, and says whether that changed them. */
  #fit(server: Limits): boolean {
    const l = { ...this.#limits };
    if (server.pushItems && server.pushItems > 0) l.pushItems = Math.min(l.pushItems, server.pushItems);
    if (server.pushBytes && server.pushBytes > 0) l.pushBytes = Math.min(l.pushBytes, server.pushBytes);
    const changed = l.pushItems !== this.#limits.pushItems || l.pushBytes !== this.#limits.pushBytes;
    this.#limits = l;
    return changed;
  }
}

/**
 * Whether a version from the server was made without seeing the one held here: the held one is
 * an edit not yet sent, made from a version before the remote one; or it is stored, and the
 * remote one, newer, was made from a version before it.
 */
function neverMet(local: Entry, remote: Item): boolean {
  const rs = remote.sequence ?? 0;
  if (local.pending) return !!local.item.base && rs > local.item.base;
  const ls = local.item.sequence ?? 0;
  return ls !== 0 && rs > ls && remote.timestamp >= local.item.timestamp && !!remote.base && remote.base < ls;
}

/** Whether two versions hold the same: then a merge makes no new edit. */
function sameContent(a: Pick<Item, 'deleted' | 'public' | 'private'>, b: Pick<Item, 'deleted' | 'public' | 'private'>): boolean {
  const part = (v: unknown) => json(v ?? undefined);
  return a.deleted === b.deleted && part(a.public) === part(b.public) && part(a.private) === part(b.private);
}

/**
 * join returns a bucket's data key, opened with one of its passphrases, for the device to keep
 * in its own key store. It reads the bucket's sealed copies and takes the first that opens and
 * whose own seal checks out under the key it holds. It throws PassphraseError if none opens.
 */
export async function join(c: Client, passphrase: string, signal?: AbortSignal): Promise<DataKey> {
  let after = 0;
  for (;;) {
    const p = await c.pull(KeysCollection, after, 0, signal);
    for (const it of p.items) {
      if (it.deleted) continue;
      try {
        valid(it);
        return await openSealedCopy(it, passphrase);
      } catch {}
    }
    if (!p.more) throw new PassphraseError();
    after = p.next;
  }
}

/**
 * create makes the data key of a new bucket and pushes its first sealed copy, under passphrase,
 * returning the key for the device to keep in its own key store. It throws HasKeyError if the
 * bucket holds any item at all, so two first runs don't make two keys.
 */
export async function create(c: Client, passphrase: string, label: string, signal?: AbortSignal): Promise<DataKey> {
  const p = await c.pull('', 0, 0, signal);
  if (p.items.length > 0) throw new HasKeyError();
  const key = DataKey.create();
  const copy = await key.sealedCopy(passphrase, label);
  copy.guid = newGUID();
  copy.timestamp = Date.now();
  const res = await c.push([key.seal(copy, true)], signal);
  const rs = res.results[0];
  if (!rs.ok) throw new Error(`create: the server didn't store the sealed copy: ${rs.code ?? ''} ${rs.error ?? ''}`);
  return key;
}
