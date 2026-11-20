import { toWire, type Item } from './item.js';
import { orDefaults, readLimits, readPage, readPushed, type Filter, type Limits, type Page, type Pushed } from './wire.js';

/** What a Client needs. */
export interface ClientOptions {
  /** The server, like https://sync.example.com/ */
  url: string;
  /** Sent as "Authorization: Bearer <token>" when set. */
  token?: string;
  /** Carries any other sign-in, such as cookies or headers; globalThis.fetch if unset. */
  fetch?: typeof globalThis.fetch;
}

/** TooLargeError is the server refusing a whole push as over its limits, which it sends along. */
export class TooLargeError extends Error {
  constructor(readonly limits: Limits, readonly reason: string) {
    super('push: too large for the server: ' + reason);
    this.name = 'TooLargeError';
  }
}

/** ServerError is a call the server answered with an error status. */
export class ServerError extends Error {
  constructor(message: string, readonly status: number) {
    super(message);
    this.name = 'ServerError';
  }
}

/** What a page may carry past its byte limit: the rest of the answer, and the one item a page always holds. */
const pageSlack = (itemBytes: number) => itemBytes + (64 << 10);

/**
 * Client speaks the protocol, version 1. Signing in is the host's business: it gives a bearer
 * token, or a fetch that adds whatever the server expects.
 */
export class Client {
  readonly url: string;
  readonly token?: string;
  readonly #fetch: typeof globalThis.fetch;

  constructor(opts: ClientOptions) {
    this.url = opts.url;
    this.token = opts.token;
    this.#fetch = opts.fetch ?? ((input, init) => globalThis.fetch(input, init));
  }

  /**
   * push sends items, as many as the server's limits allow. Each is stored unless the server
   * holds a newer version. A push over the server's limits throws a TooLargeError.
   */
  async push(items: Item[], signal?: AbortSignal): Promise<Pushed> {
    const p = readPushed(await this.#do('POST', 'v1/push', { items: items.map(toWire) }, 0, signal));
    if (p.results.length !== items.length) throw new Error(`push: ${p.results.length} results for ${items.length} items`);
    return p;
  }

  /**
   * pull returns the items the server stored after the sequence number after that pass the
   * filter; `{}` is every item. The server stops the page at pageBytes of
   * items, or its own limit if lower, but always gives at least one; zero means the default.
   */
  async pull(filter: Filter, after: number, pageBytes = 0, signal?: AbortSignal): Promise<Page> {
    const limits = orDefaults({ pageBytes });
    const q = new URLSearchParams({ after: String(after), bytes: String(limits.pageBytes) });
    for (const c of filter.collections ?? []) q.append('collection', c);
    for (const d of filter.datatypes ?? []) q.append('datatype', d);
    return readPage(await this.#do('GET', 'v1/pull?' + q, undefined, limits.pageBytes + pageSlack(limits.itemBytes), signal));
  }

  /** Makes a call, reading at most `most` bytes of the answer, or all of it if most is 0. */
  async #do(method: string, path: string, body: unknown, most: number, signal?: AbortSignal): Promise<unknown> {
    const headers: Record<string, string> = {};
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (this.token) headers['Authorization'] = 'Bearer ' + this.token;
    const resp = await this.#fetch(this.url.replace(/\/+$/, '') + '/' + path, {
      method, headers, signal, body: body === undefined ? undefined : JSON.stringify(body),
    });
    const b = await read(resp, most);
    if (b === undefined) throw new Error(`${method} ${path}: the answer is over ${most} bytes`);
    const text = new TextDecoder().decode(b);
    if (!resp.ok) {
      let e: any;
      try {
        e = JSON.parse(text);
      } catch {}
      if (resp.status === 413 && e?.limits) throw new TooLargeError(readLimits(e.limits), String(e.error ?? ''));
      const status = `${resp.status} ${resp.statusText}`.trim();
      throw new ServerError(`${method} ${path}: ${status}${e?.error ? ': ' + e.error : ''}`, resp.status);
    }
    return JSON.parse(text);
  }
}

/** Reads a body, or undefined once it is over most bytes. */
async function read(resp: Response, most: number): Promise<Uint8Array | undefined> {
  if (!most || !resp.body) return new Uint8Array(await resp.arrayBuffer());
  const reader = resp.body.getReader();
  const parts: Uint8Array[] = [];
  let n = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    n += value.length;
    if (n > most) {
      await reader.cancel();
      return undefined;
    }
    parts.push(value);
  }
  const out = new Uint8Array(n);
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}
