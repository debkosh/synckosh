// synckosh: end-to-end encrypted sync, the device side. The protocol is SPEC.md in
// https://github.com/sudiptadeb/synckosh; the server is the Go package there.

export { type Item, type WireItem, toWire, fromWire } from './item.js';
export {
  type Result, type Pushed, type Page, type Limits, DefaultLimits, orDefaults,
  RefusedTimestampAhead, RefusedTooBig, RefusedInvalid, LibraryCollection, SealedKeyDatatype, DeleteBeforeDatatype,
  deleteBeforeGUID, isDeleteBefore, valid, cutoff, validName,
} from './wire.js';
export { Clock, MaxAhead } from './clock.js';
export { newGUID } from './guid.js';
export { DataKey, openSealedCopy, PassphraseError, UnknownKeyError, NotSealedError } from './keys.js';
export { Client, type ClientOptions, TooLargeError, ServerError } from './client.js';
export { type Entry, type Local, MemoryLocal } from './local.js';
export {
  Replica, type Config, type Merger, type PutItem, create, join,
  LockedError, HasKeyError, RefusedError, RejectedError, PutError, type Invalid, type Refused, type Rejected,
} from './replica.js';
