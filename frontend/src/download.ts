import { requestResponse, type ContentVersion, type Node } from "./api.js";

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

interface DownloadAuthority {
  nodeID: number;
  revision: number;
  name: string;
  versionID: string;
  blobHash: string;
  size: number;
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
  return prepareDownload(
    session,
    {
      nodeID: node.id,
      revision: node.revision,
      name: node.name,
      versionID: node.current_version_id,
      blobHash: node.blob_hash,
      size: node.size,
    },
    signal,
    onprogress,
  );
}

export async function prepareVersionDownload(
  session: string,
  node: Node,
  version: ContentVersion,
  signal: AbortSignal,
  onprogress: (progress: DownloadProgress) => void,
): Promise<PreparedDownload> {
  if (
    node.kind !== "file" ||
    node.revision < 1 ||
    version.node_id !== node.id ||
    !version.id ||
    !version.blob_hash ||
    version.size < 0
  ) {
    throw new Error("The selected version does not have complete download authority.");
  }
  return prepareDownload(
    session,
    {
      nodeID: node.id,
      revision: node.revision,
      name: node.name,
      versionID: version.id,
      blobHash: version.blob_hash,
      size: version.size,
    },
    signal,
    onprogress,
  );
}

async function prepareDownload(
  session: string,
  authority: DownloadAuthority,
  signal: AbortSignal,
  onprogress: (progress: DownloadProgress) => void,
): Promise<PreparedDownload> {
  const response = await requestResponse("/api/daemon/web-download", session, {
    method: "POST",
    headers: {
      Accept: "application/x-ndjson",
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      node_id: authority.nodeID,
      revision: authority.revision,
      version_id: authority.versionID,
      blob_hash: authority.blobHash,
      size: authority.size,
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
          const progress = validateProgress(event, authority.size, received);
          received = progress.received;
          onprogress(progress);
          continue;
        }
        if (event.phase === "ready") {
          ready = validateReady(event, authority);
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
      ready = validateReady(event, authority);
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

function validateReady(event: DownloadEvent, authority: DownloadAuthority): PreparedDownload {
  const name = event.name;
  const versionID = event.version_id;
  const blobHash = event.blob_hash;
  const url = event.url;
  if (
    (event.received ?? 0) !== authority.size ||
    event.total !== authority.size ||
    typeof name !== "string" ||
    typeof versionID !== "string" ||
    typeof blobHash !== "string" ||
    typeof url !== "string" ||
    name !== authority.name ||
    versionID !== authority.versionID ||
    blobHash !== authority.blobHash ||
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
