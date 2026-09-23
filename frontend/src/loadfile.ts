import { sessionJSON } from "./api-transport.js";
import { hashFile, type TransferProgress } from "./upload.js";
import type { PackageImportJob, PackageImportRequest, PackagePreflight } from "./generated/docbank.js";

export interface PackageContainer {
  container_id: string;
  format: string;
  state: string;
  sha256: string;
  size: number;
}

export interface PackageChannel {
  uploadPackageContainer(
    containerID: string,
    data: Blob,
    expectedHash: string,
    signal: AbortSignal,
    onprogress: (progress: TransferProgress) => void,
  ): Promise<void>;
}

export async function uploadPackageZIP(
  session: string,
  channel: PackageChannel,
  file: File,
  containerID: string,
  signal: AbortSignal,
  onprogress: (stage: string, progress: TransferProgress) => void,
): Promise<PackageContainer> {
  if (!file.name.toLowerCase().endsWith(".zip")) throw new Error("Choose a ZIP load-file package.");
  if (file.size < 1 || file.size > 256 * 1024 ** 3) throw new Error("Choose a nonempty ZIP up to 256 GiB.");
  const digest = await hashFile(file, signal, (progress) => onprogress("Verifying source", progress));
  try {
    await packageJSON<PackageContainer>(session, "/containers", {
      container_id: containerID,
      sha256: digest,
      size: file.size,
    }, signal);
    await channel.uploadPackageContainer(containerID, file, digest, signal, (progress) =>
      onprogress("Uploading verified source", progress));
    const sealed = await packageJSON<PackageContainer>(session, `/containers/${encodeURIComponent(containerID)}/seal`, {}, signal);
    if (sealed.container_id !== containerID || sealed.sha256 !== digest || sealed.size !== file.size || sealed.state !== "sealed")
      throw new Error("The sealed container did not match the selected ZIP.");
    return sealed;
  } catch (cause) {
    // Abort only removes unfinished uploads, including when the seal response was lost.
    await sessionJSON<void>(`/api/v1/packages/containers/${encodeURIComponent(containerID)}`, {
      session, method: "DELETE", signal: AbortSignal.timeout(5000),
    }).catch(() => {});
    throw cause;
  }
}

export function packageJSON<T>(session: string, path: string, input?: unknown, signal?: AbortSignal): Promise<T> {
  return sessionJSON<T>(`/api/v1/packages${path}`, {
    session,
    method: input === undefined ? "GET" : "POST",
    headers: input === undefined ? {} : { "Content-Type": "application/json" },
    body: input === undefined ? undefined : JSON.stringify(input),
    signal,
  });
}

export async function preflightPackageZIP(
  session: string,
  containerID: string,
  profile: string,
  pageMapProfile: string,
  encoding: string,
  signal: AbortSignal,
): Promise<PackagePreflight> {
  return packageJSON(session, `/containers/${encodeURIComponent(containerID)}/preflight`, {
    profile,
    page_map_profile: pageMapProfile || undefined,
    encoding,
    source_kind: "container",
    source_ref: containerID,
  }, signal);
}

export function startPackageImport(
  session: string,
  input: PackageImportRequest,
  signal?: AbortSignal,
): Promise<PackageImportJob> {
  return packageJSON(session, "/imports", input, signal);
}

export function readPackageImport(session: string, operationID: string, signal?: AbortSignal): Promise<PackageImportJob> {
  return packageJSON(session, `/imports/${encodeURIComponent(operationID)}`, undefined, signal);
}
