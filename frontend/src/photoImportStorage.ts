import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils.js";
import { APIError } from "./api-transport.js";
import {
  getPhotoImportCheckpointHeader,
  getPhotoImportCheckpoint,
  replacePhotoImportCheckpoint,
  type PhotoImportCheckpoint,
  type PhotoCheckpointHeader,
} from "./generated/docbank.js";
import { localFilePath } from "./localFiles.js";

export type { PhotoImportCheckpoint } from "./generated/docbank.js";

export function photoImportManifest(files: File[]) {
  const manifest = files
    .map((file) => ({
      path: localFilePath(file),
      size: file.size,
      modified: file.lastModified,
    }))
    .sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  // Version 2 is JSON [version, [[path, size, mtime], ...]]. Resume compares it before any file is read.
  const canonical = JSON.stringify([
    2,
    manifest.map(({ path, size, modified }) => [path, size, modified]),
  ]);
  return { manifest, digest: bytesToHex(sha256(utf8ToBytes(canonical))) };
}

export function photoImportStorageUnavailable(cause: unknown): boolean {
  return (
    cause instanceof TypeError ||
    (cause instanceof APIError &&
      cause.status !== 401 &&
      cause.status !== 403 &&
      ["checkpoint_cookie", "checkpoint_storage"].includes(cause.code))
  );
}

export function photoImportStorage(vaultID: string, session: string) {
  async function saved(): Promise<PhotoCheckpointHeader | undefined> {
    if (!vaultID) throw new Error("The vault identity is unavailable. Reload before importing.");
    let status = await getPhotoImportCheckpointHeader({ session });
    if (!status.ready) {
      status = await getPhotoImportCheckpointHeader({ session });
      if (!status.ready)
        throw new APIError(
          "Allow cookies to save and resume photo imports.",
          409,
          "checkpoint_cookie",
        );
    }
    return status.checkpoint;
  }
  return {
    saved,
    async load(): Promise<PhotoImportCheckpoint | undefined> {
      await saved();
      return (await getPhotoImportCheckpoint({ session })).checkpoint;
    },
    async replace(next: PhotoImportCheckpoint | undefined, expected?: PhotoImportCheckpoint) {
      await saved();
      await replacePhotoImportCheckpoint(
        {
          next: next ?? null,
          expected: expected ? { sourceID: expected.sourceID, revision: expected.revision } : null,
        },
        { session },
      );
    },
  };
}
