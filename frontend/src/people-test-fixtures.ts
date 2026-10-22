import type { Person, PersonMergeReceipt, PersonSummary } from "./generated/docbank.js";

export const person = (id: string, overrides: Partial<PersonSummary> = {}): PersonSummary => ({
  person_id: id,
  display_name: id,
  display_name_folded: id,
  revision: 4,
  state: "curated",
  created_at: "2026-01-01",
  ...overrides,
});
export const personResponse = (person: PersonSummary, overrides: Partial<Person> = {}): Person => ({
  created_at: person.created_at,
  display_name: person.display_name,
  origin: "operator",
  person_id: person.person_id,
  revision: person.revision,
  state: person.state,
  updated_at: "2026-01-02",
  ...overrides,
});
export const personMergeReceipt = (
  source: PersonSummary,
  survivor: PersonSummary,
  operationID: string,
): PersonMergeReceipt => ({
  absorbed_display_name: source.display_name,
  absorbed_person_id: source.person_id,
  created_at: "2026-10-22T00:00:00Z",
  merge_id: "00000000-0000-4000-8000-000000000099",
  moved: {
    assertions: 0,
    custodian_assignments: 0,
    deduplicated_assertions: 0,
    deduplicated_identities: 0,
    external_uids: 0,
    identities: 0,
    superseded_candidates: 0,
  },
  operation_id: operationID,
  survivor_person_id: survivor.person_id,
  survivor_revision_after: survivor.revision + 1,
  survivor_revision_before: survivor.revision,
});
export const personRenameResponse = (
  person: PersonSummary,
  overrides: Partial<Person> = {},
  onResponseHeaders?: (headers: Headers) => void,
): Person => {
  const response = personResponse(person, overrides);
  onResponseHeaders?.(new Headers({ ETag: `"${response.revision}"` }));
  return response;
};
export const personMergeResponse = (
  source: PersonSummary,
  survivor: PersonSummary,
  operationID: string,
  onResponseHeaders?: (headers: Headers) => void,
): PersonMergeReceipt => {
  const response = personMergeReceipt(source, survivor, operationID);
  onResponseHeaders?.(new Headers({ ETag: `"${response.survivor_revision_after}"` }));
  return response;
};
export const ada: PersonSummary = person("ada", {
  display_name: "Ada",
  revision: 2,
  photo_summary: { library_count: 3 },
});
export const grace: PersonSummary = person("grace", {
  display_name: "Grace",
  photo_summary: { library_count: 0 },
});
