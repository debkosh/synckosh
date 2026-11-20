import { xchacha20poly1305 } from '@noble/ciphers/chacha.js';
import { argon2idAsync } from '@noble/hashes/argon2.js';
import { base32, base64, base64URL, random, unbase64, unutf8, utf8 } from './encoding.js';
import { newGUID } from './guid.js';
import { json, type Item } from './item.js';
import { KeysCollection, SealedKeyDatatype } from './wire.js';

/** PassphraseError: no sealed copy opens with this passphrase. */
export class PassphraseError extends Error {
  constructor() {
    super('wrong passphrase');
    this.name = 'PassphraseError';
  }
}

/** UnknownKeyError: the item is sealed with a key this device doesn't have. */
export class UnknownKeyError extends Error {
  constructor(what: string, readonly keyID: string) {
    super(`${what}: sealed with a key this device doesn't have (${keyID})`);
    this.name = 'UnknownKeyError';
  }
}

/** NotSealedError: the item is not sealed. */
export class NotSealedError extends Error {
  constructor(what: string) {
    super(`${what}: not sealed`);
    this.name = 'NotSealedError';
  }
}

const keySize = 32;
const nonceSize = 24;

/** @internal Seals plain under key with a random nonce: base64 of the nonce and the ciphertext. */
export function seal(key: Uint8Array, plain: Uint8Array, ad: Uint8Array): string {
  return sealWith(key, random(nonceSize), plain, ad);
}

/** @internal */
export function sealWith(key: Uint8Array, nonce: Uint8Array, plain: Uint8Array, ad: Uint8Array): string {
  const box = xchacha20poly1305(key, nonce, ad).encrypt(plain);
  const out = new Uint8Array(nonce.length + box.length);
  out.set(nonce);
  out.set(box, nonce.length);
  return base64(out);
}

/** @internal Opens what seal made; throws if it doesn't open. */
export function open(key: Uint8Array, sealed: string, ad: Uint8Array): Uint8Array {
  const b = unbase64(sealed);
  if (b.length < nonceSize) throw new Error('sealed data too short');
  return xchacha20poly1305(key, b.subarray(0, nonceSize), ad).decrypt(b.subarray(nonceSize));
}

/** An item's private part on the wire. */
interface SealedPart {
  key: string;
  box: string;
  public?: boolean;
}

/**
 * @internal What an item's seal binds outside the box: every field the device wrote, one per
 * line. Valid names hold no newline, so the text can't be read two ways.
 */
export function additional(it: Item, pub: boolean): string {
  return [it.collection, it.datatype, it.guid, String(it.timestamp), it.deleted ? '1' : '0',
    it.base ? String(it.base) : '', pub ? '1' : '0'].join('\n');
}

const what = (it: Item) => `${it.collection} ${it.guid}`;

/** The public part of a sealed copy. */
interface SealedKey {
  label: string;
  key: string;
  kdf: string;
  t: number;
  m: number;
  p: number;
  salt: string;
  box: string;
}

// Argon2id settings for new sealed copies: RFC 9106's second recommended option.
const argonTime = 3, argonMemKB = 64 * 1024, argonPar = 4;

/**
 * check refuses settings no honest writer makes, before a joining device spends time or memory
 * on them: it reads them before it can verify anything.
 */
function check(p: SealedKey): Uint8Array {
  const n = (v: unknown) => (Number.isSafeInteger(v) ? (v as number) : NaN);
  const t = n(p.t), m = n(p.m), par = n(p.p);
  if (p.kdf !== 'argon2id') throw new Error(`sealed key: kdf ${JSON.stringify(p.kdf)}, want argon2id`);
  if (!(t >= 1 && t <= 10)) throw new Error(`sealed key: t ${p.t}, want 1 to 10`);
  if (!(par >= 1 && par <= 16)) throw new Error(`sealed key: p ${p.p}, want 1 to 16`);
  if (!(m >= 8 * par && m <= 256 * 1024)) throw new Error(`sealed key: m ${p.m} KiB, want 8p to 262144`);
  let salt: Uint8Array;
  try {
    salt = unbase64(typeof p.salt === 'string' ? p.salt : '');
  } catch {
    salt = new Uint8Array(0);
  }
  if (salt.length < 8 || salt.length > 64) throw new Error('sealed key: salt, want 8 to 64 bytes');
  return salt;
}

function wrapKey(p: SealedKey, passphrase: string, salt: Uint8Array): Promise<Uint8Array> {
  return argon2idAsync(utf8(passphrase), salt, { t: p.t, m: p.m, p: p.p, dkLen: keySize });
}

/** DataKey seals every item in a bucket. */
export class DataKey {
  readonly #key: Uint8Array;

  private constructor(readonly id: string, key: Uint8Array) {
    this.#key = key;
  }

  /** create makes the key for a new bucket. */
  static create(): DataKey {
    return new DataKey('k_' + base64URL(random(8)), random(keySize));
  }

  /** from rebuilds a key the device kept. */
  static from(id: string, bytes: Uint8Array): DataKey {
    if (bytes.length !== keySize) throw new Error(`data key is ${bytes.length} bytes, want ${keySize}`);
    return new DataKey(id, Uint8Array.from(bytes));
  }

  /**
   * bytes returns the key, only for the device to keep in its own key store, such as the OS
   * keychain. It is never shown, exported or put in a QR code: a person only ever sees a
   * passphrase that opens a sealed copy.
   */
  bytes(): Uint8Array {
    return Uint8Array.from(this.#key);
  }

  /**
   * seal returns the item with its private part sealed, an empty one included, bound to every
   * other field the device wrote, so the server can't alter, forge or move it unnoticed. With
   * pub, the box also holds a copy of the public part, and a device that opens it uses that
   * copy. The library's own items, whose datatypes start with "_", are always sealed with their
   * public part.
   */
  seal(it: Item, pub = false): Item {
    pub = pub || it.datatype.startsWith('_');
    const plain = pub ? `[${json(it.public)},${json(it.private)}]` : json(it.private);
    const part: SealedPart = { key: this.id, box: seal(this.#key, utf8(plain), utf8(additional(it, pub))) };
    if (pub) part.public = true;
    return { ...it, private: part };
  }

  /**
   * open returns the item with its private part opened, and its public part taken from the box
   * when the box holds one. It throws NotSealedError for an item that isn't sealed,
   * UnknownKeyError for one sealed under another key, and an Error for a seal that doesn't open,
   * which means the item was changed after it was sealed.
   */
  open(it: Item): Item {
    const p = it.private as Partial<SealedPart> | undefined;
    if (typeof p !== 'object' || p === null || Array.isArray(p) || typeof p.key !== 'string' || !p.key ||
      typeof p.box !== 'string' || !p.box || (p.public !== undefined && typeof p.public !== 'boolean')) {
      throw new NotSealedError(what(it));
    }
    if (p.key !== this.id) throw new UnknownKeyError(what(it), p.key);
    const pub = p.public === true;
    if (!pub && it.datatype.startsWith('_')) {
      throw new Error(`${what(it)}: the library's items are sealed with their public part`);
    }
    let plain: unknown;
    try {
      plain = JSON.parse(unutf8(open(this.#key, p.box, utf8(additional(it, pub)))));
    } catch (e) {
      throw new Error(`${what(it)}: seal doesn't open: ${(e as Error).message}`);
    }
    const out: Item = { ...it };
    delete out.private;
    if (!pub) {
      if (plain !== null) out.private = plain;
      return out;
    }
    if (!Array.isArray(plain) || plain.length !== 2) {
      throw new Error(`${what(it)}: box doesn't hold the public and private parts`);
    }
    delete out.public;
    if (plain[0] !== null) out.public = plain[0];
    if (plain[1] !== null) out.private = plain[1];
    return out;
  }

  /**
   * sealedCopy seals the data key with a passphrase, as a new item for KeysCollection with its
   * own guid, to put as any item. A bucket can have several, one per passphrase.
   */
  async sealedCopy(passphrase: string, label: string): Promise<Item> {
    const salt = random(16);
    const p: SealedKey = { label, key: this.id, kdf: 'argon2id', t: argonTime, m: argonMemKB, p: argonPar,
      salt: base64(salt), box: '' };
    p.box = seal(await wrapKey(p, passphrase, salt), this.#key, utf8(this.id));
    return { collection: KeysCollection, datatype: SealedKeyDatatype, guid: newGUID(), timestamp: 0, deleted: false, public: p };
  }

  /**
   * backupCode makes a random passphrase of 128 bits, for a person to write down or scan, with
   * its sealed copy. Deleting the copy removes the code without changing the data key.
   */
  async backupCode(label: string): Promise<{ code: string; copy: Item }> {
    const code = base32(random(16)).match(/.{1,4}/g)!.join('-');
    return { code, copy: await this.sealedCopy(code, label) };
  }
}

/**
 * openSealedCopy opens a sealed copy, as the server sent it, with its passphrase. The key it
 * opens must also open the item's own seal, which proves a holder of that key wrote the copy.
 * It throws PassphraseError when the passphrase doesn't open it.
 */
export async function openSealedCopy(it: Item, passphrase: string): Promise<DataKey> {
  if (it.collection !== KeysCollection || it.datatype !== SealedKeyDatatype) throw new Error(`${what(it)}: not a sealed copy`);
  if (it.deleted) throw new Error(`${it.guid}: the sealed copy was removed`);
  const p = it.public as SealedKey;
  if (typeof p !== 'object' || p === null || Array.isArray(p)) throw new Error('sealed key: public part is not an object');
  const salt = check(p);
  let raw: Uint8Array;
  try {
    raw = open(await wrapKey(p, passphrase, salt), typeof p.box === 'string' ? p.box : '', utf8(String(p.key ?? '')));
  } catch {
    throw new PassphraseError();
  }
  const key = DataKey.from(String(p.key ?? ''), raw);
  try {
    key.open(it);
  } catch (e) {
    throw new Error(`sealed key: not sealed by its own key: ${(e as Error).message}`);
  }
  return key;
}
