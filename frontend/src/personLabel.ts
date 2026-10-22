import type { PersonSummary } from "./generated/docbank.js";

export const personDetail = (person: PersonSummary) =>
  person.identity_label || new Date(person.created_at).toLocaleDateString();

export const personLabel = (person: PersonSummary) =>
  `${person.display_name} · ${personDetail(person)}`;
