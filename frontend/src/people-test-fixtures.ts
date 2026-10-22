import type { PersonSummary } from "./generated/docbank.js";

export const person = (id: string, overrides: Partial<PersonSummary> = {}): PersonSummary => ({
  person_id: id,
  display_name: id,
  display_name_folded: id,
  revision: 4,
  state: "curated",
  created_at: "2026-01-01",
  ...overrides,
});
export const ada: PersonSummary = person("ada", {
  display_name: "Ada",
  revision: 2,
  photo_summary: { library_count: 3 },
});
export const grace: PersonSummary = person("grace", {
  display_name: "Grace",
  photo_summary: { library_count: 0 },
});
