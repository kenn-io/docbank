import type { PersonSummary } from "./generated/docbank.js";

export const personDetail = (person: PersonSummary) =>
  person.identity_label || new Date(person.created_at).toLocaleDateString();

export const personLabel = (person: PersonSummary) =>
  `${person.display_name} · ${personDetail(person)}`;

// distinctLabel adds the person ID only when a peer has the same name and
// detail, because an ID distinguishes records but rarely helps recognition.
export const distinctLabel = (person: PersonSummary, peers: readonly PersonSummary[]) =>
  peers.some(
    (peer) => peer.person_id !== person.person_id && personLabel(peer) === personLabel(person),
  )
    ? `${personLabel(person)} · ${person.person_id}`
    : personLabel(person);
