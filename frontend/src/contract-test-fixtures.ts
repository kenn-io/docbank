import type { Contract, ContractDocument, Node } from "./generated/docbank.js";

export const contractID = "11111111-1111-4111-8111-111111111111";
export const signedVersionID = "22222222-2222-4222-8222-222222222222";
export const draftVersionID = "33333333-3333-4333-8333-333333333333";

export function contractLink(overrides: Partial<ContractDocument> = {}): ContractDocument {
  return {
    node_id: 412,
    content_version_id: signedVersionID,
    blob_sha256: "a".repeat(64),
    size: 2048,
    role: "signed",
    added_at: "2027-01-02T10:00:00Z",
    current: true,
    available: true,
    ...overrides,
  };
}

export function contract(overrides: Partial<Contract> = {}): Contract {
  return {
    contract_id: contractID,
    title: "Northwind Storage - unit lease",
    contract_type: "lease",
    status: "active",
    effective_status: "active",
    starts_on: "2027-01-01",
    expires_on: "2027-12-31",
    signed_on: "2026-12-20",
    summary: "Storage unit 14 for archive boxes.",
    purpose: "",
    consideration: "Monthly fee of 120 units.",
    termination: "Either party with 60 days notice.",
    revision: 4,
    created_at: "2026-12-01T09:00:00Z",
    updated_at: "2027-01-02T10:00:00Z",
    parties: [
      {
        party_id: "44444444-4444-4444-8444-444444444444",
        role: "own",
        display_name: "Example Holdings Inc",
        kind: "organization",
        own: true,
        retired_at: null,
      },
      {
        party_id: "55555555-5555-4555-8555-555555555555",
        role: "counterparty",
        display_name: "Northwind Storage Ltd",
        kind: "organization",
        own: false,
        retired_at: null,
      },
      {
        party_id: "66666666-6666-4666-8666-666666666666",
        role: "counterparty",
        display_name: "Riverbend Analytics LLC",
        kind: "organization",
        own: false,
        retired_at: "2027-01-01T00:00:00Z",
      },
    ],
    documents: [],
    ...overrides,
  };
}

export function versionFixture(link: ContractDocument) {
  return {
    id: link.content_version_id,
    node_id: link.node_id,
    blob_hash: link.blob_sha256,
    size: link.size,
    mime_type: "application/pdf",
    created_at: "2027-01-02T10:00:00Z",
  };
}

export function nodeFixture(overrides: Partial<Node> = {}): Node {
  return {
    id: 412,
    parent_id: 7,
    name: "lease-signed.pdf",
    kind: "file",
    path: "/agreements/lease-signed.pdf",
    current_version_id: signedVersionID,
    blob_hash: "a".repeat(64),
    size: 2048,
    revision: 3,
    created_at: "2027-01-02T10:00:00Z",
    modified_at: "2027-01-02T10:00:00Z",
    ...overrides,
  } as Node;
}
