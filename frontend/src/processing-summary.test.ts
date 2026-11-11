import { describe, expect, it } from "vite-plus/test";
import type { ProcessingFlowHop } from "./generated/docbank.js";
import {
  consentLabel,
  describeClasses,
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

  it("names the vault-wide scope of automatic MP4 uploads", () => {
    const voyage = {
      capability: "embedding",
      provider_id: "voyage.multimodal-v1",
      trust_boundary: "hosted_provider",
      runtime_disclosure: {
        immediate_processor: "voyage.multimodal-v1",
        ultimate_processor: "voyage.multimodal-v1",
        endpoint: "https://api.voyageai.com",
        deployment: "9".repeat(64),
        metadata_classes: ["byte_length"],
        retained_artifact_roles: [],
      },
    } satisfies Partial<ProcessingFlowHop>;
    const video = plan("video", {
      flow: [
        hop({ ...voyage, input_classes: ["original_file"] }),
        hop({ ...voyage, input_classes: ["original_file_automatic"] }),
      ],
      disclosed_classes: ["original_file", "original_file_automatic"],
    });
    expect(whereSentences(video)).toEqual([
      "Sends this file and eligible MP4 originals throughout this vault, automatically, to Voyage (api.voyageai.com).",
    ]);
    expect(describeClasses(video.disclosed_classes)).toBe(
      "Original file, Eligible MP4 originals throughout this vault, sent automatically",
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

  it("titles the dialog by its input even when typed artifacts are not retained", () => {
    expect(purposeLabels[processingPurpose(plan("text"))].title).toBe("Extract text");
    const audio = plan("audio");
    const purpose = processingPurpose(audio, "audio/wav");
    expect(purposeLabels[purpose].title).toBe("Transcribe");
    expect(keptSentence(audio, purpose)).toBe("Saves the transcript in the vault and its backups.");
    const photo = hop({ input_classes: ["derived_upload"] });
    expect(purposeLabels[processingPurpose(plan("photos", { flow: [photo] }))].title).toBe(
      "Describe photo",
    );
    const detection = plan("faces", {
      flow: [
        hop({
          provider_id: "compatible-vision.detect-faces-v1",
          input_classes: ["derived_upload"],
        }),
      ],
    });
    expect(purposeLabels[processingPurpose(detection)].title).toBe("Find faces");
    expect(keptSentence(detection, processingPurpose(detection))).toBe(
      "Saves face boxes and measurements in the vault and its backups.",
    );

    expect(purposeLabels[processingPurpose(plan("vectors", { flow: embeddingHops }))].title).toBe(
      "Index for search",
    );
  });
});
