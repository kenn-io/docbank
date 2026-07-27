import { requestResponse, type Node } from "./api.js";

export interface DownloadProgress {
  received: number;
  total: number;
}

export interface PreparedDownload {
  url: string;
  name: string;
  versionID: string;
  blobHash: string;
  size: number;
}

interface DownloadEvent {
  phase?: string;
  received?: number;
  total?: number;
  url?: string;
  name?: string;
  version_id?: string;
  blob_hash?: string;
  detail?: string;
}

export async function prepareCurrentDownload(
  session: string,
  node: Node,
  signal: AbortSignal,
  onprogress: (progress: DownloadProgress) => void,
): Promise<PreparedDownload> {
  if (
    node.kind !== "file" ||
    !node.current_version_id ||
    !node.blob_hash ||
    node.revision < 1 ||
    node.size < 0
  ) {
    throw new Error("The selected document does not have complete download authority.");
  }
  const response = await requestResponse("/api/daemon/web-download", session, {
    method: "POST",
    headers: {
      Accept: "application/x-ndjson",
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      node_id: node.id,
      revision: node.revision,
      version_id: node.current_version_id,
      blob_hash: node.blob_hash,
      size: node.size,
    }),
    signal,
  });
  if (!response.body) throw new Error("The download response did not contain a progress stream.");

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffered = "";
  let received = 0;
  let ready: PreparedDownload | undefined;

  try {
    while (true) {
      const result = await reader.read();
      buffered += decoder.decode(result.value, { stream: !result.done });
      if (buffered.length > 64 * 1024) {
        throw new Error("The download progress response was unexpectedly large.");
      }
      const lines = buffered.split("\n");
      buffered = lines.pop() ?? "";
      for (const line of lines) {
        if (!line.trim()) continue;
        if (ready) throw new Error("The download response continued after publication.");
        const event = JSON.parse(line) as DownloadEvent;
        if (event.phase === "error") {
          throw new Error(event.detail || "Docbank could not verify this document.");
        }
        if (event.phase === "progress") {
          const progress = validateProgress(event, node.size, received);
          received = progress.received;
          onprogress(progress);
          continue;
        }
        if (event.phase === "ready") {
          ready = validateReady(event, node);
          onprogress({ received: ready.size, total: ready.size });
          continue;
        }
        throw new Error("The download response contained an unknown progress event.");
      }
      if (result.done) break;
    }
    if (buffered.trim()) {
      const event = JSON.parse(buffered) as DownloadEvent;
      if (event.phase !== "ready" || ready) {
        throw new Error("The download response ended with an invalid progress event.");
      }
      ready = validateReady(event, node);
      onprogress({ received: ready.size, total: ready.size });
    }
  } catch (cause) {
    await reader.cancel().catch(() => undefined);
    throw cause;
  }
  if (!ready) throw new Error("The download ended before Docbank published verified bytes.");
  return ready;
}

function validateProgress(
  event: DownloadEvent,
  expectedTotal: number,
  previous: number,
): DownloadProgress {
  const received = event.received ?? 0;
  const total = event.total;
  if (
    !Number.isSafeInteger(received) ||
    !Number.isSafeInteger(total) ||
    total !== expectedTotal ||
    received < previous ||
    received < 0 ||
    received > total
  ) {
    throw new Error("The download progress disagreed with document authority.");
  }
  return { received, total };
}

function validateReady(event: DownloadEvent, node: Node): PreparedDownload {
  const name = event.name;
  const versionID = event.version_id;
  const blobHash = event.blob_hash;
  const url = event.url;
  if (
    (event.received ?? 0) !== node.size ||
    event.total !== node.size ||
    typeof name !== "string" ||
    typeof versionID !== "string" ||
    typeof blobHash !== "string" ||
    typeof url !== "string" ||
    name !== node.name ||
    versionID !== node.current_version_id ||
    blobHash !== node.blob_hash ||
    !url?.startsWith("/api/daemon/web-download/file?ticket=")
  ) {
    throw new Error("The verified download disagreed with the selected document.");
  }
  return {
    url,
    name,
    versionID,
    blobHash,
    size: event.total,
  };
}

export function offerPreparedDownload(download: PreparedDownload): void {
  const link = document.createElement("a");
  link.href = download.url;
  link.download = download.name;
  link.rel = "noreferrer";
  link.hidden = true;
  document.body.append(link);
  link.click();
  link.remove();
}
