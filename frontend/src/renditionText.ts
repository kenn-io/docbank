import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { requestJSON, requestResponse } from "./api.js";
import type { SelectedSource } from "./selectedSource.js";

export type RenditionTextState = "ready" | "verified_empty" | "failed" | "unprocessed" |
  "unconfigured" | "profile_required" | "historical_unavailable";

export interface RenditionObservation {
  configuration: "configured" | "unconfigured" | "profile_required";
  profileFingerprint?: string;
  generationID?: string;
  coverageState?: "complete" | "partial" | "failed" | "unprocessed" | "none" | "unavailable";
  attachmentID?: string;
  buildID?: string;
}

export interface ReadyRenditionText {
  state: "ready";
  profile: { name: string; configuration: "configured"; fingerprint: string };
  generationID: string;
  attachmentID: string;
  buildID: string;
  artifact: { id: string; sha256: string; size: number; mediaType: string };
}

export type RenditionTextResolution = ReadyRenditionText | {
  state: Exclude<RenditionTextState, "ready">;
  profile: { name: string; configuration: RenditionObservation["configuration"]; fingerprint: string };
  generationID: string;
  attachmentID: string;
  buildID: string;
};

const sha = /^[0-9a-f]{64}$/;
const states = new Set<RenditionTextState>([
  "ready", "verified_empty", "failed", "unprocessed", "unconfigured", "profile_required",
  "historical_unavailable",
]);
const configurations = new Set<RenditionObservation["configuration"]>([
  "configured", "unconfigured", "profile_required",
]);
const maxTextBytes = 16 * 1024 * 1024;

function malformed(reason: string): never {
  throw new Error(`Malformed rendition text receipt: ${reason}.`);
}

function object(value: unknown, field: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) malformed(`${field} must be an object`);
  return value as Record<string, unknown>;
}

function exactKeys(value: Record<string, unknown>, required: readonly string[], optional: readonly string[], field: string): void {
  const allowed = new Set([...required, ...optional]);
  if (required.some((key) => !Object.hasOwn(value, key)) || Object.keys(value).some((key) => !allowed.has(key))) {
    malformed(`${field} has missing or unknown fields`);
  }
}

function safeInteger(value: unknown, field: string, minimum: number): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < minimum) malformed(`${field} is invalid`);
  return value;
}

function string(value: unknown, field: string): string {
  if (typeof value !== "string" || /[\uD800-\uDFFF]/u.test(value)) malformed(`${field} is invalid`);
  return value;
}

export async function resolveRenditionText(
  session: string,
  source: SelectedSource,
  authorizationRevision: number,
  profileName: string,
  observed: RenditionObservation | undefined,
  signal: AbortSignal,
): Promise<RenditionTextResolution> {
  const raw = object(await requestJSON<unknown>("/api/v1/renditions/text", session, {
    method: "POST", headers: { "Content-Type": "application/json" }, signal,
    body: JSON.stringify({
      node_id: source.nodeID, revision: authorizationRevision, version_id: source.versionID,
      blob_hash: source.blobHash, size: source.size,
      ...(profileName ? { profile: profileName } : {}),
      ...(observed ? { observed: {
        configuration: observed.configuration,
        ...(observed.profileFingerprint ? { profile_fingerprint: observed.profileFingerprint } : {}),
        ...(observed.generationID ? { generation_id: observed.generationID } : {}),
        ...(observed.coverageState ? { coverage_state: observed.coverageState } : {}),
        ...(observed.attachmentID ? { attachment_id: observed.attachmentID } : {}),
        ...(observed.buildID ? { build_id: observed.buildID } : {}),
      } } : {}),
    }),
  }), "receipt");
  exactKeys(raw, ["state", "source", "profile", "generation_id", "attachment_id", "build_id"], ["$schema", "artifact"], "receipt");
  if (raw.$schema !== undefined && string(raw.$schema, "receipt.$schema").length === 0) {
    malformed("receipt.$schema is invalid");
  }

  const state = string(raw.state, "state") as RenditionTextState;
  if (!states.has(state)) malformed("state is unknown");
  const receivedSource = object(raw.source, "source");
  exactKeys(receivedSource, ["node_id", "revision", "version_id", "blob_hash", "size", "media_type"], [], "source");
  if (safeInteger(receivedSource.node_id, "source.node_id", 1) !== source.nodeID ||
      safeInteger(receivedSource.revision, "source.revision", 1) !== authorizationRevision ||
      string(receivedSource.version_id, "source.version_id") !== source.versionID ||
      string(receivedSource.blob_hash, "source.blob_hash") !== source.blobHash ||
      safeInteger(receivedSource.size, "source.size", 0) !== source.size ||
      string(receivedSource.media_type, "source.media_type") !== source.mimeType) {
    malformed("source does not match the selected version");
  }

  const receivedProfile = object(raw.profile, "profile");
  exactKeys(receivedProfile, ["name", "configuration", "fingerprint"], [], "profile");
  const name = string(receivedProfile.name, "profile.name");
  const configuration = string(receivedProfile.configuration, "profile.configuration") as RenditionObservation["configuration"];
  const fingerprint = string(receivedProfile.fingerprint, "profile.fingerprint");
  if (!configurations.has(configuration) || (profileName && name !== profileName) ||
      (fingerprint !== "" && !sha.test(fingerprint)) ||
      (observed && (configuration !== observed.configuration ||
        (observed.profileFingerprint !== undefined && fingerprint !== observed.profileFingerprint)))) {
    malformed("profile does not match the accepted selection");
  }
  const generationID = identity(raw.generation_id, "generation_id");
  const attachmentID = identity(raw.attachment_id, "attachment_id");
  const buildID = identity(raw.build_id, "build_id");
  if ((observed?.generationID && generationID !== observed.generationID) ||
      (observed?.attachmentID && attachmentID !== observed.attachmentID) ||
      (observed?.buildID && buildID !== observed.buildID)) {
    malformed("rendition identities do not match the accepted snapshot");
  }
  const profile = { name, configuration, fingerprint };
  if (state !== "ready") {
    if (raw.artifact !== undefined) malformed("non-ready state contains an artifact");
    return { state, profile, generationID, attachmentID, buildID };
  }
  if (configuration !== "configured" || !sha.test(fingerprint)) malformed("ready state has no configured profile");
  const receivedArtifact = object(raw.artifact, "artifact");
  exactKeys(receivedArtifact, ["id", "sha256", "size", "media_type"], [], "artifact");
  const id = string(receivedArtifact.id, "artifact.id");
  const artifactHash = string(receivedArtifact.sha256, "artifact.sha256");
  const size = safeInteger(receivedArtifact.size, "artifact.size", 0);
  const mediaType = string(receivedArtifact.media_type, "artifact.media_type");
  if ([...id].length < 1 || [...id].length > 128 || !sha.test(artifactHash) || size > maxTextBytes ||
      mediaType !== "text/markdown; charset=utf-8" || !attachmentID || !buildID) {
    malformed("artifact authority is invalid");
  }
  return { state, profile: { name, configuration, fingerprint }, generationID, attachmentID, buildID,
    artifact: { id, sha256: artifactHash, size, mediaType } };
}

function identity(value: unknown, field: string): string {
  const result = string(value, field);
  if (result !== "" && !sha.test(result)) malformed(`${field} is invalid`);
  return result;
}

export async function readVerifiedRenditionText(
  session: string,
  source: SelectedSource,
  authorizationRevision: number,
  ready: ReadyRenditionText,
  signal: AbortSignal,
): Promise<string> {
  const query = new URLSearchParams({ node_id: String(source.nodeID), revision: String(authorizationRevision),
    version_id: source.versionID, blob_hash: source.blobHash, size: String(source.size),
    profile_fingerprint: ready.profile.fingerprint, attachment_id: ready.attachmentID,
    build_id: ready.buildID, artifact_id: ready.artifact.id });
  if (ready.generationID) query.set("generation_id", ready.generationID);
  const response = await requestResponse(`/api/v1/renditions/text/content?${query}`, session, {
    headers: { Accept: "text/markdown" }, signal,
  });
  const headers = response.headers;
  if (headers.get("Content-Type")?.toLowerCase() !== ready.artifact.mediaType ||
      headers.get("Content-Length") !== String(ready.artifact.size) ||
      headers.get("X-Docbank-Rendition-SHA256") !== ready.artifact.sha256 ||
      headers.get("X-Docbank-Rendition-Size") !== String(ready.artifact.size) ||
      headers.get("X-Docbank-Rendition-Profile") !== ready.profile.fingerprint ||
      headers.get("X-Docbank-Rendition-Generation") !== ready.generationID ||
      headers.get("X-Docbank-Rendition-Attachment") !== ready.attachmentID ||
      headers.get("X-Docbank-Rendition-Build") !== ready.buildID ||
      headers.get("X-Docbank-Rendition-Artifact") !== ready.artifact.id) {
    throw new Error("The received text disagreed with the selected rendition.");
  }
  const bytes = await readExactBody(response, ready.artifact.size);
  const computed = bytesToHex(sha256(bytes));
  if (computed !== ready.artifact.sha256 || !digestMatches(headers, computed)) {
    throw new Error("The received text disagreed with the selected rendition.");
  }
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    throw new Error("The verified text rendition is not valid UTF-8.");
  }
}

async function readExactBody(response: Response, expectedSize: number): Promise<Uint8Array> {
  if (!response.body) throw new Error("The text response did not contain rendition bytes.");
  const bytes = new Uint8Array(expectedSize);
  const reader = response.body.getReader();
  let received = 0;
  try {
    while (true) {
      const next = await reader.read();
      if (next.done) break;
      if (received + next.value.length > expectedSize) throw new Error("The received text disagreed with the selected rendition.");
      bytes.set(next.value, received);
      received += next.value.length;
    }
  } catch (cause) {
    await reader.cancel().catch(() => undefined);
    throw cause;
  }
  if (received !== expectedSize) throw new Error("The received text disagreed with the selected rendition.");
  return bytes;
}

function digestMatches(headers: Headers, expected: string): boolean {
  const match = /^sha-256=:([A-Za-z0-9+/]+={0,2}):$/.exec(headers.get("Content-Digest") ?? "");
  if (!match) return false;
  try {
    return [...atob(match[1] ?? "")].map((value) => value.charCodeAt(0).toString(16).padStart(2, "0")).join("") === expected;
  } catch {
    return false;
  }
}
