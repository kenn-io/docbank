// Package upload turns a locally inspected capability into a one-shot
// provider upload. It copies exact source bytes into a private spool, syncs
// them, independently reopens and verifies them, removes their pathname, and
// owns cleanup until the reader closes.
package upload
