import { requestJSON } from "./api.js";
import { hashFile, type TransferProgress } from "./upload.js";

export interface MailboxChunk {
  index: number;
  sha256: string;
  size: number;
}
export interface MailboxContainer {
  id: string;
  sha256: string;
  size: number;
  format: "mbox" | "zip";
  state: string;
  manifest_sha256: string;
  chunks: MailboxChunk[];
}
export interface MailboxSettings {
  dialect: string;
  destination_id: number;
  label_tags: Record<string, string>;
  recipe?: string;
}
export interface MailboxJob {
  id: string;
  container_id: string;
  container_sha256: string;
  settings: MailboxSettings;
  state: string;
  collection_id: string;
  imported: number;
  rejected: number;
  retries: number;
  pending: number;
  canceled: number;
  checkpoint: number;
  scanned_tail: boolean;
  reason: string;
}
export interface MailboxPreview {
  dialect: string;
  entry_count: number;
  entries: { name: string; size: number; sha256: string }[];
  samples: { sequence: number; eml_size: number; rejection?: string }[];
  has_more: boolean;
}
export interface MailboxChannel {
  uploadMailboxChunk(
    containerID: string,
    index: number,
    data: Blob,
    hash: string,
    signal: AbortSignal,
    onprogress: (progress: TransferProgress) => void,
  ): Promise<void>;
}
export function mailboxJSON<T>(
  session: string,
  path: string,
  input?: unknown,
  signal?: AbortSignal,
): Promise<T> {
  return requestJSON<T>(`/api/v1/mailbox${path}`, session, {
    method: input === undefined ? "GET" : "POST",
    headers: input === undefined ? {} : { "Content-Type": "application/json" },
    body: input === undefined ? undefined : JSON.stringify(input),
    signal,
  });
}
export async function uploadMailboxArchive(
  session: string,
  channel: MailboxChannel,
  file: File,
  id: string,
  signal: AbortSignal,
  onprogress: (stage: string, progress: TransferProgress) => void,
): Promise<MailboxContainer> {
  if (file.size < 1 || file.size > 256 * 1024 ** 3)
    throw new Error("Select a nonempty archive up to 256 GiB.");
  const hash = await hashFile(file, signal, (p) => onprogress("Verifying source", p));
  const format = file.name.toLowerCase().endsWith(".zip") ? "zip" : "mbox";
  const c = await mailboxJSON<MailboxContainer>(
    session,
    "/containers",
    { id, sha256: hash, size: file.size, format },
    signal,
  );
  if (c.id !== id || c.sha256 !== hash || c.size !== file.size || c.format !== format)
    throw new Error("Container identity did not match the selected file.");
  const chunks: MailboxChunk[] = [];
  const chunkBytes = 64 * 1024 ** 2;
  for (let offset = 0, index = 0; offset < file.size; offset += chunkBytes, index++) {
    const data = file.slice(offset, Math.min(file.size, offset + chunkBytes));
    const digest = await hashFile(new File([data], "chunk"), signal, () => {});
    chunks.push({ index, sha256: digest, size: data.size });
    await channel.uploadMailboxChunk(id, index, data, digest, signal, (p) =>
      onprogress("Uploading verified source", {
        processed: offset + p.processed,
        total: file.size,
      }),
    );
  }
  onprogress("Sealing source", { processed: file.size, total: file.size });
  const sealed = await mailboxJSON<MailboxContainer>(
    session,
    `/containers/${encodeURIComponent(id)}/seal`,
    {},
    signal,
  );
  const manifest = JSON.stringify({ sha256: hash, size: file.size, chunks });
  const manifestHash = await hashFile(new File([manifest], "manifest"), signal, () => {});
  if (
    sealed.id !== id ||
    sealed.sha256 !== hash ||
    sealed.size !== file.size ||
    sealed.state !== "sealed" ||
    sealed.manifest_sha256 !== manifestHash
  )
    throw new Error(
      "The sealed container did not match the independently hashed source and ordered chunks.",
    );
  return sealed;
}
