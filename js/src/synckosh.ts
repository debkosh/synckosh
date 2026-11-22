// synckosh: end-to-end encrypted sync, the device side. The protocol is SPEC.md in
// https://github.com/sudiptadeb/synckosh; the server is the Go package there.

export { type Item, type WireItem, toWire, fromWire } from './item.js';
export {
  type Result, type Pushed, type Page, type Limits, DefaultLimits, orDefaults,
  RefusedTimestampAhead, RefusedTooBig, RefusedInvalid, RefusedIdentityTaken, LibraryCollection, SealedKeyDatatype, DeleteBeforeDatatype,
  deleteBeforeGUID, isDeleteBefore, valid, cutoff, validName, type Filter,
} from './wire.js';
export { Clock, MaxAhead } from './clock.js';
export { canonical, sameContent, identity } from './canonical.js';
export { newGUID } from './guid.js';
export { DataKey, openSealedCopy, readCopy, normalizeCode, codeBits, type CopyInfo, PassphraseError, UnknownKeyError, NotSealedError } from './keys.js';
export { Client, type ClientOptions, TooLargeError, ServerError } from './client.js';
export { type Entry, type Local, MemoryLocal } from './local.js';
export {
  Replica, type Config, type Merger, type PutItem, create, join,
  look, type BucketState, type Passphrase, NoKeyError, NoCopyError,
  LockedError, HasKeyError, BucketResetError, RefusedError, RejectedError, PutError, type Invalid, type Refused, type Rejected,
} from './replica.js';
