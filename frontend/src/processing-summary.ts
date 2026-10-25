import type { ProcessingFlowHop, ProcessingPlan } from "./generated/docbank.js";

export type ProcessingPurpose = "extract" | "transcribe" | "describe" | "index";

export const processingSetupURL = "https://docbank.ai/docs/usage/configuration/";

export const purposeLabels: Record<
  ProcessingPurpose,
  { title: string; running: string; done: string }
> = {
  extract: {
    title: "Extract text",
    running: "Extracting…",
    done: "Text extracted.",
  },
  transcribe: {
    title: "Transcribe",
    running: "Transcribing…",
    done: "Transcribed.",
  },
  describe: {
    title: "Describe photo",
    running: "Describing…",
    done: "Suggestion ready.",
  },
  index: { title: "Index for search", running: "Indexing…", done: "Indexed." },
};

export function processingPurpose(plan: ProcessingPlan): ProcessingPurpose {
  const rendition = plan.flow.find((hop) => hop.capability === "rendition");
  if (!rendition) return "index";
  if (rendition.input_classes.includes("derived_upload")) return "describe";
  if (rendition.runtime_disclosure.retained_artifact_roles.includes("provider_transcript"))
    return "transcribe";
  return "extract";
}

const providerNames: Record<string, string> = {
  docling: "Docling",
  mistral: "Mistral",
  "openai-compatible": "an OpenAI-compatible service",
  "compatible-vision": "a vision service",
  voyage: "Voyage",
};

/** Names a provider by the family prefix of its descriptor ID. */
function providerName(providerID: string): string {
  const family = providerID.split(".")[0] ?? providerID;
  return providerNames[family] ?? family;
}

function endpointHost(endpoint: string): string {
  try {
    return new URL(endpoint).host || endpoint;
  } catch {
    return endpoint;
  }
}

const inputWords: Record<string, string> = {
  original_file: "this file",
  filename: "its name",
  derived_upload: "a small preview",
  rendition_chunk: "this file's text",
  query_text: "your searches",
  query_and_excerpt: "your searches with matching excerpts",
};

function joinWords(words: string[]): string {
  if (words.length <= 1) return words[0] ?? "";
  if (words.length === 2) return `${words[0]} and ${words[1]}`;
  return `${words.slice(0, -1).join(", ")}, and ${words.at(-1)}`;
}

interface Destination {
  name: string;
  host: string;
  boundary: string;
  sends: string[];
}

/** Groups every hop that leaves this process by where it goes. */
export function externalDestinations(plan: ProcessingPlan): Destination[] {
  const destinations = new Map<string, Destination>();
  for (const hop of plan.flow) {
    if (hop.trust_boundary === "local_process") continue;
    const name = providerName(hop.provider_id);
    const host = endpointHost(hop.runtime_disclosure.endpoint);
    const key = `${name}\0${host}\0${hop.trust_boundary}`;
    const destination = destinations.get(key) ?? {
      name,
      host,
      boundary: hop.trust_boundary,
      sends: [],
    };
    for (const input of hopInputs(hop)) {
      const word = inputWords[input] ?? input.replaceAll("_", " ");
      if (!destination.sends.includes(word)) destination.sends.push(word);
    }
    destinations.set(key, destination);
  }
  return [...destinations.values()];
}

function hopInputs(hop: ProcessingFlowHop): string[] {
  return hop.disclose_filename && !hop.input_classes.includes("filename")
    ? [...hop.input_classes, "filename"]
    : hop.input_classes;
}

/** One sentence per place data goes; local-only plans say nothing leaves. */
export function whereSentences(plan: ProcessingPlan): string[] {
  const destinations = externalDestinations(plan);
  if (destinations.length === 0) return ["Runs on this computer. Nothing leaves Docbank."];
  return destinations.map((destination) => {
    const place =
      destination.boundary === "operator_network"
        ? `${destination.name} on your network (${destination.host})`
        : `${destination.name} (${destination.host})`;
    return `Sends ${joinWords(destination.sends)} to ${place}.`;
  });
}

export function keptSentence(plan: ProcessingPlan, purpose: ProcessingPurpose): string {
  if (purpose === "describe") return "Saves a suggested caption and tags for you to review.";
  const kept: string[] = [];
  if (plan.retained_classes.includes("sanitized_markdown"))
    kept.push(purpose === "transcribe" ? "the transcript" : "the extracted text");
  else if (plan.retained_classes.includes("normalized_evidence")) kept.push("a search index");
  if (plan.retained_classes.includes("embedding_vector_set")) kept.push("search vectors");
  if (kept.length === 0) return "Saves nothing after the run.";
  return `Saves ${joinWords(kept)} in the vault and its backups.`;
}

export function consentLabel(profile: string, plan: ProcessingPlan): string {
  const names = [...new Set(externalDestinations(plan).map((destination) => destination.name))];
  if (names.length === 0) return `Allow “${profile}” to process documents in this vault`;
  return `Allow “${profile}” to send documents from this vault to ${joinWords(names)}`;
}

const ineligibleReasons: Record<string, string> = {
  empty_source: "This file is empty.",
  source_bytes_exceeded: "This file is too large for this profile.",
  pdf_password_required: "This PDF is locked. Add its password to the PDF password file.",
  encrypted_container: "This file is encrypted.",
  malformed_input: "This file looks damaged.",
  semantic_units_exceeded: "This file has more pages than this profile allows.",
  preview_missing: "This photo has no preview yet.",
  preview_unavailable: "This photo has no preview yet.",
};

export function ineligibleSentence(reason: string): string {
  return (
    ineligibleReasons[reason] ??
    `This profile can't process this file (${reason.replaceAll("_", " ")}).`
  );
}

const classWords: Record<string, string> = {
  original_file: "Original file",
  filename: "File name",
  derived_upload: "Preview image",
  rendition_chunk: "Extracted text",
  query_text: "Search text",
  query_and_excerpt: "Search text and excerpts",
  sanitized_markdown: "Extracted text",
  provider_markdown: "Provider text",
  normalized_evidence: "Search index",
  typed_artifacts: "Provider files",
  embedding_vector_set: "Search vectors",
  structured_evidence: "Page layout",
  provider_transcript: "Transcript",
  byte_length: "size",
  content_hash: "checksum",
  detected_media_type: "file type",
  synthetic_filename: "generic name",
  sanitized_filename: "file name",
  query_role: "search role",
};

export function describeClasses(classes: string[]): string {
  if (classes.length === 0) return "Nothing";
  const words = classes.map((item) => classWords[item] ?? item.replaceAll("_", " "));
  const text = words.join(", ");
  return text.charAt(0).toUpperCase() + text.slice(1);
}
