// Package store provides the local, producer-controlled SQLite ledger.
//
// The ledger records run metadata, canonical lifecycle events, output counts,
// and explicitly submitted findings connected to closed runs. It deliberately
// does not capture raw process output, contact a network service, sign records,
// or claim that consistency checks establish authenticity.
package store
