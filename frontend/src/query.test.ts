import { describe, expect, it } from "vitest";
import formatMetadata from "../../document/format_metadata.json";
import fixture from "../../internal/query/testdata/identity-vectors.json";
import {
  canonicalHighlightSet,
  canonicalQuery,
  classifyMedia,
  highlightSetFingerprint,
  parseHighlightSet,
  parseQuery,
  queryFingerprint,
} from "./query.js";

describe("query identity", () => {
  for (const vector of fixture.queries) {
    it(vector.name, async () => {
      const value = parseQuery(vector.input_json);
      expect(canonicalQuery(value)).toBe(vector.canonical_utf8);
      expect(await queryFingerprint(value)).toBe(vector.fingerprint);
    });
  }

  for (const testCase of fixture.invalid_queries) {
    it(`rejects ${testCase.name}`, () => {
      expect(() => parseQuery(testCase.json_text)).toThrow();
    });
  }

  it("accepts null only for optional filters", () => {
    expect(canonicalQuery(parseQuery('{"filters":{"paths":null,"size_min":null,"no_tags":true}}')))
      .toBe('{"filters":{"no_tags":true},"mode":"lexical","sort":{"direction":"asc","field":"name"},"syntax":"simple","text":"","v":1}');
    expect(() => parseQuery('{"sort":null}')).toThrow();
  });

  it("accepts a valid escaped astral scalar", () => {
    expect(parseQuery('{"text":"\\ud83d\\ude00"}').text).toBe("😀");
  });

  it("rejects invalid values and bounds", () => {
    const invalid = [
      '{"filters":{"paths":["relative"]}}',
      '{"filters":{"tag_ids":["00000000-0000-3000-8000-000000000001"]}}',
      '{"filters":{"mime_types":["text/*"]}}',
      '{"filters":{"extensions":[".pdf"]}}',
      '{"v":0}',
      '{"syntax":""}',
      '{"sort":{"field":""}}',
      '{"filters":{"modified_after":""}}',
      '{"filters":{"modified_after":"2026-09-10T00:00:00Z","modified_before":"2026-09-09T00:00:00Z"}}',
      '{"filters":{"modified_after":"2026-09-09T00:00:00.1Z","modified_before":"2026-09-09T00:00:00Z"}}',
      '{"filters":{"size_min":2,"size_max":1}}',
      `{"text":"${"😀".repeat(8193)}"}`,
      `${" ".repeat(128 * 1024)}{}`,
    ];
    for (const raw of invalid) expect(() => parseQuery(raw)).toThrow();
    expect(() => parseQuery('{"filters":{"modified_after":"2026-09-09T00:00:00Z","modified_before":"2026-09-09T00:00:00.1Z"}}')).not.toThrow();
  });

  it("enforces the shared normalized query byte bound during parse", () => {
    const raw = `{"filters":{"paths":["/${"x".repeat(fixture.bounds.oversized_query_path_ascii_bytes)}"]}}`;
    expect(new TextEncoder().encode(raw).byteLength).toBeLessThan(128 * 1024);
    expect(() => parseQuery(raw)).toThrow();
  });
});

describe("highlight-set identity", () => {
  for (const vector of fixture.highlight_sets) {
    it(vector.name, async () => {
      const value = parseHighlightSet(vector.input_json);
      expect(canonicalHighlightSet(value)).toBe(vector.canonical_utf8);
      expect(await highlightSetFingerprint(value)).toBe(vector.fingerprint);
    });
  }

  for (const testCase of fixture.invalid_highlight_sets) {
    it(`rejects ${testCase.name}`, () => {
      expect(() => parseHighlightSet(testCase.json_text)).toThrow();
    });
  }

  it("rejects query payloads, duplicates, unknowns, and bounds", () => {
    const invalid = [
      '{"v":1,"text":"query"}',
      '{"v":1,"terms":[]}',
      '{"v":1,"terms":[{"text":"same","color":"#ffffff"},{"text":"same","color":"#000000"}]}',
      '{"v":1,"terms":[{"text":"term","color":"#ABCDEF"}]}',
      '{"v":1,"terms":[{"text":"term","color":"#abcdef","regex":true}]}',
      `{"v":1,"terms":[{"text":"${"😀".repeat(257)}","color":"#abcdef"}]}`,
    ];
    for (const raw of invalid) expect(() => parseHighlightSet(raw)).toThrow();
  });

  it("enforces shared highlight boundaries during parse", () => {
    const maxTerm = `{"v":1,"terms":[{"text":"${"x".repeat(fixture.bounds.highlight_max_term_scalars)}","color":"#abcdef"}]}`;
    expect(() => parseHighlightSet(maxTerm)).not.toThrow();
    const maxTerms = Array.from({ length: fixture.bounds.highlight_max_terms }, (_, index) =>
      `{"text":"${String(index).padStart(2, "0")}","color":"#abcdef"}`);
    expect(() => parseHighlightSet(`{"v":1,"terms":[${maxTerms.join(",")}]}`)).not.toThrow();

    const oversizedTerms = Array.from({ length: fixture.bounds.highlight_max_terms }, (_, index) =>
      `{"text":"${"😀".repeat(fixture.bounds.oversized_highlight_astral_scalars)}${String(index).padStart(2, "0")}","color":"#abcdef"}`);
    const oversized = `{"v":1,"terms":[${oversizedTerms.join(",")}]}`;
    expect(new TextEncoder().encode(oversized).byteLength).toBeLessThan(128 * 1024);
    expect(() => parseHighlightSet(oversized)).toThrow();
  });
});

describe("media classification", () => {
  for (const testCase of fixture.media_cases) {
    it(testCase.name, () => {
      expect(classifyMedia(testCase.media_type, testCase.filename)).toBe(testCase.family);
    });
  }

  it("uses every document-owned mapping", () => {
    for (const metadata of formatMetadata) {
      expect(classifyMedia(metadata.media_type, "synthetic.bin"), metadata.id).toBe(metadata.query_family);
      for (const extension of metadata.extensions) {
        expect(classifyMedia("application/octet-stream", `synthetic.${extension}`), metadata.id).toBe(metadata.query_family);
      }
    }
  });

  it.each([
    ["message/rfc822", "synthetic.eml", "email"],
    ["application/pdf", "synthetic.pdf", "document"],
    ["text/csv", "synthetic.csv", "spreadsheet"],
    ["application/vnd.ms-powerpoint", "synthetic.ppt", "presentation"],
    ["image/vnd.synthetic", "synthetic.bin", "image"],
    ["video/vnd.synthetic", "synthetic.bin", "audio_video"],
    ["text/plain", "synthetic.txt", "text"],
    ["text/x-python", "synthetic.py", "source_code"],
    ["text/html", "synthetic.html", "web"],
    ["text/calendar", "synthetic.ics", "calendar"],
    ["application/zip", "synthetic.zip", "archive"],
    ["image/vnd.dwg", "synthetic.dwg", "cad"],
    ["application/vnd.synthetic-unknown", "synthetic.pdf", "unknown"],
  ])("classifies %s as %s", (mediaType, filename, family) => {
    expect(classifyMedia(mediaType, filename)).toBe(family);
  });

  it("uses extension only for missing or generic MIME", () => {
    expect(classifyMedia("application/octet-stream", "REPORT.PDF")).toBe("document");
    expect(classifyMedia("application/octet-stream", "scan.PNG")).toBe("image");
    expect(classifyMedia("application/octet-stream", "recording.MP3")).toBe("audio_video");
    expect(classifyMedia("", "main.GO")).toBe("source_code");
    expect(classifyMedia("application/vnd.synthetic-unknown", "report.pdf")).toBe("unknown");
  });
});
