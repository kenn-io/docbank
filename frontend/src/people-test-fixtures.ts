import type { PersonSummary } from "./generated/docbank.js";

export const ada: PersonSummary = {
  person_id: "ada",
  display_name: "Ada",
  display_name_folded: "ada",
  revision: 2,
  state: "curated",
  created_at: "2026-01-01",
  photo_summary: { library_count: 3, hidden_count: 1 },
};
export const grace: PersonSummary = {
  ...ada,
  person_id: "grace",
  display_name: "Grace",
  display_name_folded: "grace",
  revision: 4,
  photo_summary: { library_count: 0 },
};
export const person = (id: string, overrides: Partial<PersonSummary> = {}): PersonSummary => ({
  ...grace,
  person_id: id,
  display_name: id,
  display_name_folded: id,
  ...overrides,
});
