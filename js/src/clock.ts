/**
 * MaxAhead is how far ahead of this device's clock, in milliseconds, a timestamp it reads may be
 * and still move its clock forward: an hour. The server refuses items that far ahead.
 */
export const MaxAhead = 60 * 60 * 1000;

/**
 * Clock makes timestamps, in milliseconds, that are always after every one this device has
 * made or read, so an edit made after seeing another always wins over it, whatever the wall
 * clock says.
 */
export class Clock {
  #last = 0;
  readonly #now: () => number;

  /** `now` is the wall clock in milliseconds, Date.now unless a test gives another. */
  constructor(now: () => number = Date.now) {
    this.#now = now;
  }

  /**
   * now returns a timestamp after every one this clock has made or observed: the wall clock, or
   * one millisecond past the newest it has seen when the wall clock is behind it.
   */
  now(): number {
    this.#last = Math.max(this.#now(), this.#last + 1);
    return this.#last;
  }

  /**
   * observe moves the clock up to a timestamp read from another device, unless that timestamp
   * is more than MaxAhead ahead of this device's clock.
   */
  observe(t: number): void {
    if (t <= this.#now() + MaxAhead) this.#last = Math.max(this.#last, t);
  }

  /**
   * reset moves the clock back to the server's time t, when the server refused a timestamp as
   * too far ahead. Once this device's wall clock is right, its next timestamps are accepted.
   */
  reset(t: number): void {
    this.#last = Math.min(this.#last, t);
  }

  /** last returns the newest timestamp this clock has made or observed, for the device to keep. */
  last(): number {
    return this.#last;
  }
}
