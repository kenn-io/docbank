import { APIError } from "./api-transport.js";
import { photoEvents } from "./generated/docbank.js";

export interface PhotoEvent {
  asset_id?: string;
  revision?: number;
  refetch?: boolean;
}

export interface PhotoEventBatch {
  assets: Map<string, number>;
  refetch: boolean;
}

const emptyBatch = (): PhotoEventBatch => ({
  assets: new Map(),
  refetch: false,
});

// The server sends a keepalive every 15 seconds; silence past this means the connection died.
export const photoEventIdleTimeout = 45_000;

// SSE frames can cross any network chunk boundary, including CRLF pairs.
export async function readPhotoEvents(
  body: ReadableStream<Uint8Array>,
  signal: AbortSignal,
  onchange: (event: PhotoEvent) => void,
  idleTimeout = photoEventIdleTimeout,
): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let line = "";
  let data: string[] = [];
  let event = "";
  let cr = false;
  const finishLine = () => {
    if (!line) {
      if (data.length && (!event || event === "change"))
        onchange(JSON.parse(data.join("\n")) as PhotoEvent);
      data = [];
      event = "";
    } else if (!line.startsWith(":")) {
      const colon = line.indexOf(":");
      const field = colon < 0 ? line : line.slice(0, colon);
      const value = colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "");
      if (field === "data") data.push(value);
      if (field === "event") event = value;
    }
    line = "";
  };
  const abort = () => {
    void reader.cancel().catch(() => {});
  };
  const read = () => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const stalled = new Promise<never>((_, reject) => {
      timer = setTimeout(
        () => reject(new Error("Photo event stream stopped responding")),
        idleTimeout,
      );
    });
    return Promise.race([reader.read(), stalled]).finally(() => clearTimeout(timer));
  };
  signal.addEventListener("abort", abort, { once: true });
  try {
    while (!signal.aborted) {
      const chunk = await read();
      if (chunk.done) break;
      for (const char of decoder.decode(chunk.value, { stream: true })) {
        if (cr && char === "\n") {
          cr = false;
          continue;
        }
        cr = char === "\r";
        if (char === "\r" || char === "\n") finishLine();
        else line += char;
        if (line.length + data.reduce((sum, part) => sum + part.length, 0) > 64 * 1024)
          throw new Error("Photo event exceeds frame limit");
      }
    }
  } finally {
    signal.removeEventListener("abort", abort);
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

// One visible tab holds the stream; other tabs use their own audience cursor.
const photoEventLock = "docbank-photo-events";
type PhotoEventOptions = NonNullable<Parameters<typeof photoEvents>[0]> & { signal: AbortSignal };

async function consumePhotoEvents(
  options: PhotoEventOptions,
  schedule: (event: PhotoEvent) => void,
  connected: () => void,
): Promise<void> {
  const consume = async (handoff = false) => {
    const connecting = new AbortController();
    const deadline = setTimeout(
      () => connecting.abort(new Error("Photo event stream did not respond")),
      photoEventIdleTimeout,
    );
    try {
      const response = await photoEvents({
        ...options,
        signal: AbortSignal.any([options.signal, connecting.signal]),
      });
      clearTimeout(deadline);
      if (options.signal.aborted) return;
      if (!response.body || !response.headers.get("Content-Type")?.startsWith("text/event-stream"))
        throw new Error("Photo event stream unavailable");
      connected();
      if (handoff) schedule({ refetch: true });
      await readPhotoEvents(response.body, options.signal, schedule);
    } finally {
      clearTimeout(deadline);
    }
  };
  let held = false;
  await navigator.locks.request(photoEventLock, { ifAvailable: true }, async (lock) => {
    if (!lock) return;
    held = true;
    await consume();
  });
  if (!held && !options.signal.aborted)
    await navigator.locks.request(photoEventLock, { signal: options.signal }, () => consume(true));
}

export function watchPhotoEvents(
  session: string,
  refresh: (batch: PhotoEventBatch) => Promise<void>,
  onfailure: (cause: unknown) => void,
  onconnected: () => void = () => {},
  onrefreshed: () => void = () => {},
  pollCursor?: (receive: (cursor?: string) => void) => void,
): () => void {
  const polling = typeof navigator.locks === "undefined";
  let established = false;
  let cursor: string | undefined;
  let stopped = false;
  let controller: AbortController | undefined;
  let retry: ReturnType<typeof setTimeout> | undefined;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let pending = emptyBatch();
  let failures = 0;
  let refreshing = false;
  let refreshFailures = 0;
  let refreshAfter = 0;
  const flush = async () => {
    timer = undefined;
    if (stopped || document.visibilityState !== "visible" || refreshing || !hasPending()) return;
    const batch = pending;
    pending = emptyBatch();
    refreshing = true;
    try {
      await refresh(batch);
      refreshFailures = 0;
      refreshAfter = Date.now() + 1000;
      if (!stopped) onrefreshed();
    } catch (cause) {
      if (stopped) return;
      pending = { ...emptyBatch(), refetch: true };
      refreshAfter = Date.now() + Math.min(30_000, 1000 * 2 ** Math.min(refreshFailures++, 5));
      onfailure(cause);
      if (cause instanceof APIError && cause.status === 401) {
        dispose();
        return;
      }
    } finally {
      refreshing = false;
      if (hasPending() && !stopped) schedule();
    }
  };
  const hasPending = () => pending.refetch || pending.assets.size > 0;
  const schedule = (event?: PhotoEvent) => {
    if (event && (!pending.refetch || pending.assets.size > 0)) {
      if (event.refetch) pending.refetch = true;
      if (event.asset_id)
        pending.assets.set(
          event.asset_id,
          Math.max(event.revision ?? 0, pending.assets.get(event.asset_id) ?? 0),
        );
      if (!event.asset_id || pending.assets.size > 64) {
        pending.refetch = true;
        pending.assets.clear();
      }
    }
    if (!stopped && !refreshing && document.visibilityState === "visible")
      timer ??= setTimeout(() => void flush(), Math.max(100, refreshAfter - Date.now()));
  };
  const connect = async () => {
    if (stopped || document.visibilityState !== "visible") return;
    if (polling) {
      onconnected();
      return;
    }
    const current = new AbortController();
    controller = current;
    let connectedAt: number | undefined;
    try {
      await consumePhotoEvents(
        {
          session,
          signal: current.signal,
          cache: "no-store",
        },
        schedule,
        () => {
          established = true;
          onconnected();
          connectedAt = Date.now();
        },
      );
    } catch (cause) {
      if (cause instanceof APIError && (cause.status === 401 || cause.status === 403)) {
        if (!current.signal.aborted && !stopped) {
          onfailure(cause);
          dispose();
        }
        return;
      }
      if (cause instanceof APIError && cause.status === 429 && !current.signal.aborted && !stopped)
        onfailure(cause);
    } finally {
      if (controller === current) {
        controller = undefined;
        established = false;
      }
    }
    if (connectedAt !== undefined && Date.now() - connectedAt >= 30_000) failures = 0;
    if (!stopped && !current.signal.aborted)
      retry = setTimeout(() => void connect(), Math.min(30_000, 1000 * 2 ** failures++));
  };
  const visibility = () => {
    established = false;
    controller?.abort();
    clearTimeout(retry);
    clearTimeout(timer);
    retry = undefined;
    timer = undefined;
    if (document.visibilityState === "visible") {
      void connect();
      if (hasPending()) schedule();
    }
  };
  pollCursor?.((next) => {
    if (next && next !== cursor) {
      cursor = next;
      if (!established) schedule({ refetch: true });
    }
  });
  document.addEventListener("visibilitychange", visibility);
  void connect();
  const dispose = () => {
    stopped = true;
    established = false;
    controller?.abort();
    clearTimeout(retry);
    clearTimeout(timer);
    document.removeEventListener("visibilitychange", visibility);
  };
  return dispose;
}
