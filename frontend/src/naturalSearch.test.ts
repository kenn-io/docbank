import { describe, expect, it } from "vitest";
import {
  directFileEvidenceNote,
  evidenceKindLabels,
  naturalSearchFallbackNote,
  naturalSearchModes,
  naturalSearchRequest,
  naturalSearchRerank,
  rerankingNote,
  selectNaturalSearchProfile,
} from "./naturalSearch.js";
import type { ProcessingProfileSummary } from "./generated/docbank.js";

const profile = (name: string, bindings: string[], reranking_available = false, queryBindings = bindings): ProcessingProfileSummary => ({
  name,
  fingerprint: "a".repeat(64),
  rendition: true,
  embedding_bindings: bindings,
  query_embedding_bindings: queryBindings,
  reranking_available,
});

describe("natural search mapping", () => {
  it("chooses the first named profile with an embedding binding", () => {
    expect(selectNaturalSearchProfile([
      profile("zeta", ["embed"]),
      profile("alpha", []),
      profile("beta", ["embed"]),
    ])?.name).toBe("beta");
  });

  it("keeps Auto lexical when no embedding binding exists", () => {
    const noBinding = profile("private", [], true);
    expect(naturalSearchModes(noBinding).map((option) => option.value)).toEqual(["names", "auto", "lexical"]);
    expect(naturalSearchRequest("auto", noBinding)).toEqual({ mode: "auto" });
    expect(naturalSearchRequest("lexical", noBinding)).toEqual({ mode: "lexical" });
    expect(naturalSearchRerank(noBinding, "lexical", true)).toBe(true);
    expect(naturalSearchRequest("semantic", noBinding)).toBeUndefined();
  });

  it("maps embedding-backed Auto to hybrid and omits rerank when off", () => {
    const withBinding = profile("private", ["embed"], true);
    expect(naturalSearchRequest("auto", withBinding)).toEqual({ mode: "hybrid", binding_id: "embed" });
    expect(naturalSearchRequest("hybrid", withBinding)).toEqual({ mode: "hybrid", binding_id: "embed" });
    expect(naturalSearchRerank(withBinding, "hybrid", false)).toBeUndefined();
  });

  it("keeps processing-only bindings out of natural search", () => {
    const directOnly = profile("direct", ["direct-file"], false, []);
    const mixed = profile("mixed", ["direct-file", "semantic"], false, ["semantic"]);
    expect(selectNaturalSearchProfile([directOnly, mixed])?.name).toBe("mixed");
    expect(naturalSearchModes(directOnly).map((option) => option.value)).toEqual(["names", "auto", "lexical"]);
    expect(naturalSearchRequest("auto", directOnly)).toEqual({ mode: "auto" });
    expect(naturalSearchRequest("hybrid", directOnly)).toBeUndefined();
    expect(naturalSearchRequest("auto", mixed)).toEqual({ mode: "hybrid", binding_id: "semantic" });
  });

  it("labels evidence and degradation states", () => {
    expect(evidenceKindLabels(["embedding", "rendition_segment", "embedding"])).toEqual(["Semantic", "Document text"]);
    expect(directFileEvidenceNote()).toContain("no text excerpt");
    expect(rerankingNote("degraded", "timed_out")).toContain("timed_out");
    expect(naturalSearchFallbackNote("search failed")).toContain("Names and text");
  });
});
