import { describe, expect, it } from "vite-plus/test";
import {
  consentLabel,
  keptSentence,
  processingPurpose,
  purposeLabels,
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

  it("titles the dialog by what the profile produces", () => {
    expect(purposeLabels[processingPurpose(plan("text"))].title).toBe("Extract text");
    const transcript = hop({
      runtime_disclosure: {
        ...hop().runtime_disclosure,
        retained_artifact_roles: ["provider_transcript"],
      },
    });
    expect(purposeLabels[processingPurpose(plan("audio", { flow: [transcript] }))].title).toBe(
      "Transcribe",
    );
    const photo = hop({ input_classes: ["derived_upload"] });
    expect(purposeLabels[processingPurpose(plan("photos", { flow: [photo] }))].title).toBe(
      "Describe photo",
    );
    expect(purposeLabels[processingPurpose(plan("vectors", { flow: embeddingHops }))].title).toBe(
      "Index for search",
    );
  });
});
