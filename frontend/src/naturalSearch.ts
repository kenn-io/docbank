import type { DocumentSearchRequestMode, ProcessingProfileSummary } from "./generated/docbank.js";

export type NaturalSearchMode = "names" | "auto";

export interface NaturalSearchRequestMode {
  mode: DocumentSearchRequestMode;
  binding_id?: string;
}

function naturalQueryBindings(profile: ProcessingProfileSummary): string[] {
  return profile.query_embedding_bindings ?? profile.embedding_bindings;
}

export function selectNaturalSearchProfile(
  profiles: ProcessingProfileSummary[],
): ProcessingProfileSummary | undefined {
  return (
    [...profiles]
      .sort((left, right) => left.name.localeCompare(right.name))
      .find((profile) => naturalQueryBindings(profile).length > 0) ??
    [...profiles].sort((left, right) => left.name.localeCompare(right.name))[0]
  );
}

export function naturalSearchPlaceholder(syntax: "simple" | "advanced"): string {
  return syntax === "advanced" ? "name:invoice AND tag:urgent" : "Search names and text";
}

export function naturalSearchRequest(
  mode: NaturalSearchMode,
  profile: ProcessingProfileSummary,
): NaturalSearchRequestMode | undefined {
  if (mode === "names") return undefined;
  const bindingID = naturalQueryBindings(profile)[0];
  return bindingID ? { mode: "hybrid", binding_id: bindingID } : { mode: "lexical" };
}

export function naturalSearchRerank(
  profile: ProcessingProfileSummary | undefined,
  mode: NaturalSearchMode,
  enabled: boolean,
): boolean | undefined {
  return enabled && mode !== "names" && profile?.reranking_available ? true : undefined;
}

function evidenceKindLabel(kind: string): string {
  switch (kind) {
    case "node_name":
      return "Name";
    case "content_blob":
      return "Text";
    case "rendition_segment":
      return "Document text";
    case "embedding":
      return "Semantic";
    default:
      return "Evidence";
  }
}

export function evidenceKindLabels(kinds: string[]): string[] {
  return [...new Set(kinds)].map(evidenceKindLabel);
}

export function rerankingNote(outcome: string, cause = ""): string {
  switch (outcome) {
    case "applied":
      return "Reranked results applied.";
    case "skipped":
      return "Reranking skipped because no candidates were available.";
    case "degraded":
      return `Reranking unavailable (${cause || "provider unavailable"}). Base results remain.`;
    default:
      return "Reranking failed. Base results remain.";
  }
}

export function naturalSearchFallbackNote(cause: string): string {
  const detail = cause.trim().slice(0, 128);
  return detail
    ? `Natural-language search unavailable: ${detail}. Showing names and text.`
    : "Natural-language search unavailable. Showing names and text.";
}
