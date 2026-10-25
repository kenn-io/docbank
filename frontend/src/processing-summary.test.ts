import { describe, expect, it } from "vite-plus/test";
import {
  consentLabel,
  keptSentence,
  processingPurpose,
  purposeTitle,
  whereSentences,
} from "./processing-summary.js";
import { doclingHop, embeddingHops, hop, plan } from "./processing-test-fixtures.js";

describe("processing summary", () => {
  it("says nothing leaves when every step runs in process", () => {
    const local = plan("text");
    expect(whereSentences(local)).toEqual(["Runs on this computer. Nothing leaves Docbank."]);
    expect(keptSentence(local, processingPurpose(local))).toBe(
      "Saves the extracted text in the vault and its backups.",
    );
    expect(consentLabel("text", local)).toBe("Allow “text” to process documents in this vault");
  });

  it("names a hosted provider, its host, and the disclosed file name", () => {
    const hosted = plan("docling", { flow: [doclingHop] });
    expect(whereSentences(hosted)).toEqual([
      "Sends this file and its name to Docling (docling.example.com).",
    ]);
    expect(consentLabel("docling", hosted)).toBe(
      "Allow “docling” to send documents from this vault to Docling",
    );
  });

  it("lists only the steps that leave this process, merged by destination", () => {
    const mixed = plan("private_text", {
      flow: [hop(), ...embeddingHops],
      retained_classes: ["sanitized_markdown", "normalized_evidence", "embedding_vector_set"],
    });
    expect(whereSentences(mixed)).toEqual([
      "Sends this file's text and your searches to an OpenAI-compatible service on your network (127.0.0.1:9999).",
    ]);
    expect(keptSentence(mixed, "extract")).toBe(
      "Saves the extracted text and search vectors in the vault and its backups.",
    );
  });

  it("treats an unrecognized boundary as leaving the machine", () => {
    const unknown = plan("odd", {
      flow: [
        hop({
          trust_boundary: "somewhere",
          provider_id: "acme.ocr-v1",
          runtime_disclosure: { ...hop().runtime_disclosure, endpoint: "https://ocr.acme.test" },
        }),
      ],
    });
    expect(whereSentences(unknown)).toEqual(["Sends this file to acme (ocr.acme.test)."]);
  });

  it("titles the dialog by what the profile produces", () => {
    expect(purposeTitle(processingPurpose(plan("text")))).toBe("Extract text");
    const transcript = hop({
      runtime_disclosure: {
        ...hop().runtime_disclosure,
        retained_artifact_roles: ["provider_transcript"],
      },
    });
    expect(purposeTitle(processingPurpose(plan("audio", { flow: [transcript] })))).toBe(
      "Transcribe",
    );
    const photo = hop({ input_classes: ["derived_upload"] });
    expect(purposeTitle(processingPurpose(plan("photos", { flow: [photo] })))).toBe(
      "Describe photo",
    );
    expect(purposeTitle(processingPurpose(plan("vectors", { flow: embeddingHops })))).toBe(
      "Index for search",
    );
  });
});
