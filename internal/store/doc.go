// Package store provides the local, producer-controlled SQLite ledger.
//
// The ledger records run metadata, event metadata, canonical JSON event
// payloads, and output counts. It deliberately does not capture raw process
// output, contact a network service, sign records, or claim that consistency
// checks establish authenticity.
package store
