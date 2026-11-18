import { copyItem, type Item } from './item.js';

/**
 * Entry is an item as a device keeps it: opened, and marked pending until the server has it. A
 * locked entry is one sealed under a key this device doesn't hold, kept as it came. `sent` is
 * the timestamp the entry was last pushed with, until the device sees the server's answer: a
 * version with that timestamp is this device's own push, even when the answer was lost.
 */
export interface Entry {
  item: Item;
  pending?: boolean;
  locked?: boolean;
  sent?: number;
}

/**
 * Local is a device's own store. It is the real copy of the data: if the server loses anything,
 * devices push it back from here. Every method hands out copies: the replica changes what it
 * gets, and nothing it gets may change the store until it puts it back. MemoryLocal is one; a
 * browser app keeps one in IndexedDB, a Node app in a file or database.
 */
export interface Local {
  get(collection: string, guid: string): Promise<Entry | undefined>;
  /** Stores entries, each replacing its collection and guid's, all together. */
  put(entries: Entry[]): Promise<void>;
  remove(collection: string, guids: string[]): Promise<void>;
  /** The entries of a collection, or of every collection if collection is empty, in guid order. */
  entries(collection: string): Promise<Entry[]>;
  /** The entries of every collection that the server doesn't have yet. */
  pending(): Promise<Entry[]>;
  /** A value the replica keeps, undefined if unset. */
  meta(key: string): Promise<string | undefined>;
  /** Sets a value, or clears it with undefined. */
  setMeta(key: string, value: string | undefined): Promise<void>;
}

const copy = (e: Entry): Entry => ({ ...e, item: copyItem(e.item) });

/** MemoryLocal is a Local in memory, for tests and for apps that keep their data elsewhere. */
export class MemoryLocal implements Local {
  readonly #items = new Map<string, Map<string, Entry>>();
  readonly #meta = new Map<string, string>();

  async get(collection: string, guid: string): Promise<Entry | undefined> {
    const e = this.#items.get(collection)?.get(guid);
    return e && copy(e);
  }

  async put(entries: Entry[]): Promise<void> {
    for (const e of entries) {
      let c = this.#items.get(e.item.collection);
      if (!c) this.#items.set(e.item.collection, (c = new Map()));
      c.set(e.item.guid, copy(e));
    }
  }

  async remove(collection: string, guids: string[]): Promise<void> {
    const c = this.#items.get(collection);
    for (const g of guids) c?.delete(g);
  }

  async entries(collection: string): Promise<Entry[]> {
    const names = collection ? [collection] : [...this.#items.keys()].sort();
    const out: Entry[] = [];
    for (const name of names) {
      const c = this.#items.get(name);
      if (!c) continue;
      for (const guid of [...c.keys()].sort()) out.push(copy(c.get(guid)!));
    }
    return out;
  }

  async pending(): Promise<Entry[]> {
    return (await this.entries('')).filter((e) => e.pending);
  }

  async meta(key: string): Promise<string | undefined> {
    return this.#meta.get(key);
  }

  async setMeta(key: string, value: string | undefined): Promise<void> {
    if (value === undefined) this.#meta.delete(key);
    else this.#meta.set(key, value);
  }
}
