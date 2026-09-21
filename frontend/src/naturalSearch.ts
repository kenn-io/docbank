import type { DocumentSearchRequestMode, ProcessingProfileSummary } from "./generated/docbank.js";

export type NaturalSearchMode = "names" | "auto" | "lexical" | "semantic" | "hybrid";

export interface NaturalSearchModeOption {
  value: NaturalSearchMode;
  label: string;
}

export interface NaturalSearchRequestMode {
  mode: DocumentSearchRequestMode;
  binding_id?: string;
}

const modeLabels: Record<NaturalSearchMode, string> = {
  names: "Names and text",
  auto: "Auto",
  lexical: "Lexical",
  semantic: "Semantic",
  hybrid: "Hybrid",
};

export function selectNaturalSearchProfile(
  profiles: ProcessingProfileSummary[],
): ProcessingProfileSummary | undefined {
  return [...profiles].sort((left, right) => left.name.localeCompare(right.name))
    .find((profile) => profile.embedding_bindings.length > 0)
    ?? [...profiles].sort((left, right) => left.name.localeCompare(right.name))[0];
}

export function naturalSearchModes(
  profile: ProcessingProfileSummary | undefined,
): NaturalSearchModeOption[] {
  const modes: NaturalSearchMode[] = ["names"];
  if (!profile) return modes.map((value) => ({ value, label: modeLabels[value] }));
  modes.push("auto", "lexical");
  if (profile.embedding_bindings.length > 0) modes.push("semantic", "hybrid");
  return modes.map((value) => ({ value, label: modeLabels[value] }));
}

export function naturalSearchModeLabel(mode: NaturalSearchMode): string {
  return modeLabels[mode];
}

export function naturalSearchRequest(
  mode: NaturalSearchMode,
  profile: ProcessingProfileSummary,
): NaturalSearchRequestMode | undefined {
  if (mode === "names") return undefined;
  const bindingID = profile.embedding_bindings[0];
  if ((mode === "semantic" || mode === "hybrid") && !bindingID) return undefined;
  if (mode === "auto" && bindingID) return { mode: "hybrid", binding_id: bindingID };
  if ((mode === "semantic" || mode === "hybrid") && bindingID) return { mode, binding_id: bindingID };
  return { mode: mode as DocumentSearchRequestMode };
}

export function naturalSearchRerank(
  profile: ProcessingProfileSummary | undefined,
  mode: NaturalSearchMode,
  enabled: boolean,
): boolean | undefined {
  return enabled && mode !== "names" && profile?.reranking_available ? true : undefined;
}

export function evidenceKindLabel(kind: string): string {
  switch (kind) {
    case "node_name": return "Name";
    case "content_blob": return "Text";
    case "rendition_segment": return "Document text";
    case "embedding": return "Semantic";
    default: return "Evidence";
  }
}

export function evidenceKindLabels(kinds: string[]): string[] {
  return [...new Set(kinds)].map(evidenceKindLabel);
}

export function rerankingNote(outcome: string, cause = ""): string {
  switch (outcome) {
    case "applied": return "Reranked results applied.";
    case "skipped": return "Reranking skipped because no candidates were available.";
    case "degraded": return `Reranking unavailable (${cause || "provider unavailable"}). Base results remain.`;
    default: return "Reranking failed. Base results remain.";
  }
}

export function naturalSearchFallbackNote(cause: string): string {
  const detail = cause.trim().slice(0, 128);
  return detail
    ? `Natural-language search unavailable: ${detail}. Showing Names and text.`
    : "Natural-language search unavailable. Showing Names and text.";
}

export function directFileEvidenceNote(): string {
  return "Direct-file result; no text excerpt.";
}
