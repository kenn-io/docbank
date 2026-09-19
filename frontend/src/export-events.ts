import { getExportJobEvents, type GetExportJobEvents200 as ExportProgressEvent, type GetExportJobEventsParams } from "./generated/docbank.js";

/** Read one progress stream and release its body when iteration stops. */
export async function* streamExportJobEvents(
  id: string,
  params?: GetExportJobEventsParams,
  options?: Parameters<typeof getExportJobEvents>[2],
): AsyncGenerator<ExportProgressEvent, void> {
  const response = await getExportJobEvents(id, params, options);
  if (!response.body) throw new Error("The export response did not contain a progress stream.");
  const reader = response.body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let buffered = "";
  try {
    while (true) {
      const { done, value } = await reader.read();
      buffered += decoder.decode(value, { stream: !done });
      const lines = buffered.split("\n");
      buffered = lines.pop() ?? "";
      for (const line of lines) {
        if (line.trim()) yield JSON.parse(line) as ExportProgressEvent;
      }
      if (done) {
        if (buffered.trim()) yield JSON.parse(buffered) as ExportProgressEvent;
        return;
      }
    }
  } finally {
    await reader.cancel().catch(() => undefined);
    reader.releaseLock();
  }
}
