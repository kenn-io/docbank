import { getExportJobEvents, type GetExportJobEvents200 as ExportProgressEvent, type GetExportJobEventsParams } from "./generated/docbank.js";

/** Read one progress stream and release its body when iteration stops. */
export async function* streamExportJobEvents(
  id: string,
  params?: GetExportJobEventsParams,
  options?: Parameters<typeof getExportJobEvents>[2],
): AsyncGenerator<ExportProgressEvent, void> {
  const response = await getExportJobEvents(id, params, options);
  if (!response.body || response.headers.get("Content-Type")?.split(";", 1)[0] !== "application/x-ndjson") {
    throw new Error("The export response did not contain a progress stream.");
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let buffered = "";
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) {
        buffered += decoder.decode();
        if (buffered.length) throw new Error("The export progress stream was truncated.");
        return;
      }
      // Bound individual events even when a network read contains many lines.
      for (let offset = 0; offset < value.length; offset += 4096) {
        buffered += decoder.decode(value.subarray(offset, offset + 4096), { stream: true });
        let newline: number;
        while ((newline = buffered.indexOf("\n")) >= 0) {
          const line = buffered.slice(0, newline);
          buffered = buffered.slice(newline + 1);
          if (line.length > 64 * 1024) throw new Error("The export progress event was unexpectedly large.");
          if (line.trim()) yield JSON.parse(line) as ExportProgressEvent;
        }
        if (buffered.length > 64 * 1024) throw new Error("The export progress event was unexpectedly large.");
      }
    }
  } finally {
    await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
}
