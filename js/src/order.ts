// Order keys sort items in a list. An item keeps its own order key in its data, so adding or
// moving one item changes only that item. Two devices may give two items the same key; a list
// shows its items by order key, then by guid, which sorts by when each was made.

/** The order-key alphabet, in the order it sorts as bytes. */
const digits = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz';

/**
 * NoRoomError is between's answer when no key sorts strictly between the two it was given: they
 * are equal or out of order, or b is a followed only by "0".
 */
export class NoRoomError extends Error {
  constructor() {
    super('no order key sorts between these two');
    this.name = 'NoRoomError';
  }
}

/**
 * between returns an order key that sorts after a and before b. An empty a means the start of
 * the list, an empty b its end. The keys it makes never end in "0", so there is always room
 * between two of them; keys from elsewhere may not leave room, and then it throws NoRoomError.
 * A key with a character outside 0-9, A-Z and a-z is an error too.
 */
export function between(a: string, b: string): string {
  for (const k of [a, b]) {
    for (const c of k) {
      if (!digits.includes(c)) throw new Error(`order key ${JSON.stringify(k)}: ${JSON.stringify(c)} is not an order-key digit`);
    }
  }
  if (b !== '' && (a >= b || (b.startsWith(a) && /^0*$/.test(b.slice(a.length))))) throw new NoRoomError();
  let out = '';
  let bounded = b !== '';
  for (let i = 0; ; i++) {
    const lo = i < a.length ? digits.indexOf(a[i]) : 0;
    const hi = bounded ? digits.indexOf(b[i]) : digits.length;
    if (hi - lo > 1) return out + digits[(lo + hi) >> 1];
    out += digits[lo];
    if (hi > lo) bounded = false;
  }
}

/** less is how a list sorts: by order key, then by guid. */
export function less(orderA: string, guidA: string, orderB: string, guidB: string): boolean {
  return orderA !== orderB ? orderA < orderB : guidA < guidB;
}
