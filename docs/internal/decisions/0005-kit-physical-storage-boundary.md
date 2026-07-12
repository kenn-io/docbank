# ADR-0005: Kit owns physical storage mechanics; docbank owns policy

- **Status:** Accepted
- **Date:** 2026-07-11
- **Decision source:** shared packed-storage extraction and Kit v0.7 adoption

## Context

Docbank and msgvault both need crash-safe loose and packed content-addressed
storage. Duplicating reader caching, staging recovery, pack replacement, and
repacking would create two subtly different physical lifecycle engines.
Application reachability rules are not shared, however.

## Decision

`go.kenn.io/kit/packstore` owns durable loose publication, mixed reads, reader
retirement, reconciliation, packing, unpacking, repacking, and physical
deletion ordering. Docbank owns its SQLite catalog adapter, transactional
mapping changes, reachability, retention, commands, and daemon policy.

A row in docbank's `blobs` table grants physical read authority. Kit never
infers liveness from docbank tables.

## Consequences

- Existing loose vaults open without conversion.
- Large blobs can remain loose while small blobs become packed.
- Physical maintenance fixes belong in Kit; product policy stays in docbank.
- GC can remove packed catalog authority without falsely reporting immutable
  pack ranges as reclaimed disk space.

## Alternatives rejected

- Fork msgvault storage into docbank: duplicates the hardest crash-sensitive
  code.
- Put application SQL and liveness in Kit: couples shared mechanics to one
  product's schema.

## Public architecture

[Packed Storage](../../architecture/packed-storage.md)
