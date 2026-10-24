import { APIError, sessionResponse } from "./api-transport.js";
import {
  announcePhotoEvent,
  onPhotoEventSignal,
  withPhotoEventOwnership,
} from "./photoEventOwner.js";

export type PhotoEvent = { asset_id?: string };

/** The photos changed since the last pass, or an ID-free change that could have touched any of them. */
export type PhotoChange = {
  ids: ReadonlySet<string>;
  idFree: boolean;
  resume?: boolean;
  listRefreshed?: boolean;
};

export const affectsPhoto = (change: PhotoChange, id: string) =>
  change.idFree || change.ids.has(id);

/** Waits for every read, then throws the first failure, so a pass never starts over a read still running. */
export async function settleReads(reads: (Promise<unknown> | undefined)[]) {
  const failed = (await Promise.allSettled(reads)).find((result) => result.status === "rejected");
  if (failed) throw failed.reason;
}

function parsePhotoEvent(value: unknown): PhotoEvent {
  if (!value || typeof value !== "object" || Array.isArray(value)) return {};
  const event = value as Record<string, unknown>;
  if (Object.keys(event).some((key) => key !== "asset_id")) return {};
  if (
    event.asset_id !== undefined &&
    (typeof event.asset_id !== "string" || event.asset_id.length === 0)
  )
    return {};
  return event as PhotoEvent;
}

export const photoEventIdleTimeout = 45_000;
const frameLimit = 65536;

export async function readPhotoEvents(
  body: ReadableStream<Uint8Array>,
  signal: AbortSignal,
  receive: (event: PhotoEvent) => void,
  timeout = photoEventIdleTimeout,
) {
  const reader = body.getReader(),
    decoder = new TextDecoder();
  let buffered = "";
  const cancel = () => void reader.cancel().catch(() => {});
  signal.addEventListener("abort", cancel, { once: true });
  try {
    while (!signal.aborted) {
      const timer = setTimeout(cancel, timeout);
      const chunk = await reader.read().finally(() => clearTimeout(timer));
      if (chunk.done) return;
      buffered += decoder.decode(chunk.value, { stream: true });
      let match: RegExpExecArray | null;
      while ((match = /\r?\n\r?\n|\r\r/.exec(buffered))) {
        if (match.index > frameLimit) throw new Error("Photo event exceeds frame limit");
        const data = [...buffered.slice(0, match.index).matchAll(/^data: ?(.*)$/gm)]
          .map((line) => line[1])
          .join("\n");
        if (data) {
          let value: unknown;
          try {
            value = JSON.parse(data);
          } catch {
            value = undefined;
          }
          receive(parsePhotoEvent(value));
        }
        buffered = buffered.slice(match.index + match[0].length);
      }
      if (buffered.length > frameLimit) throw new Error("Photo event exceeds frame limit");
    }
  } finally {
    signal.removeEventListener("abort", cancel);
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

async function openPhotoEvents(session: string, signal: AbortSignal) {
  const connecting = new AbortController(),
    deadline = setTimeout(() => connecting.abort(), photoEventIdleTimeout);
  try {
    const response = await sessionResponse("/api/v1/photos/events", {
      session,
      headers: { Accept: "text/event-stream" },
      signal: AbortSignal.any([signal, connecting.signal]),
    });
    if (!response.body) throw new Error("Photo event stream unavailable");
    return response.body;
  } finally {
    clearTimeout(deadline);
  }
}

const passSpacing = 1000;
const maxChangedIDs = 64;

/**
 * Keeps open photo views current. Events only mark the watcher dirty; one pass at a time
 * runs `refresh` to completion and runs again, at least a second (or twice the pass's duration) later,
 * if anything changed meanwhile. A read that finished is never thrown away, so every pass progresses
 * however fast changes arrive. `refresh` returns false to ask for the pass again later.
 */
export function watchPhotoEvents(
  session: string,
  refresh: (change: PhotoChange) => Promise<boolean | void>,
  failure: (cause: unknown) => void,
  opened: () => void = () => {},
) {
  let stopped = false,
    running = false,
    dirty = false,
    idFree = false,
    failures = 0,
    connections = 0,
    resumable = false,
    newer = false,
    notBefore = 0;
  let ids = new Set<string>();
  let controller = new AbortController();
  let pass: ReturnType<typeof setTimeout> | undefined,
    retry: ReturnType<typeof setTimeout> | undefined;
  const schedule = () => {
    if (stopped || running || !dirty || document.visibilityState !== "visible") return;
    clearTimeout(pass);
    pass = setTimeout(() => void run(), Math.max(0, notBefore - Date.now()));
  };
  // More changed photos than a pass can name, like any ID-free change, ask for a full refetch.
  const queue = (change: PhotoChange) => {
    dirty = true;
    idFree ||= change.idFree;
    for (const id of change.ids) ids.add(id);
    if (idFree || ids.size > maxChangedIDs) {
      idFree = true;
      ids = new Set();
    }
    schedule();
  };
  const mark = (event: PhotoEvent = {}) => {
    newer = true;
    resumable = false;
    queue({ ids: new Set(event.asset_id ? [event.asset_id] : []), idFree: !event.asset_id });
  };
  // A failed pass stays queued; with nothing new since, its retry may keep the pages it read.
  const requeue = (change: PhotoChange) => {
    queue(change);
    resumable = !newer;
  };
  const run = async () => {
    pass = undefined;
    if (stopped || running || !dirty || document.visibilityState !== "visible") return;
    const change = { ids, idFree, resume: resumable || undefined };
    ids = new Set();
    idFree = dirty = newer = false;
    running = true;
    const began = Date.now();
    let delay = passSpacing;
    try {
      if ((await refresh(change)) === false) {
        queue(change);
        resumable = false;
      } else {
        failures = 0;
        resumable = false;
      }
    } catch {
      requeue(change);
      delay = Math.min(30_000, passSpacing * 2 ** failures++);
    } finally {
      running = false;
      // A slow pass earns a proportionally longer rest, so a steady import cannot keep lists reloading.
      notBefore = Date.now() + Math.max(delay, 2 * (Date.now() - began));
      schedule();
    }
  };
  const connect = async () => {
    const current = controller;
    try {
      await withPhotoEventOwnership(current.signal, async (signal) => {
        const body = await openPhotoEvents(session, signal);
        connections = 0;
        opened();
        // The server subscribes before it sends headers and replays nothing, so refetch what came earlier.
        announcePhotoEvent({});
        mark();
        await readPhotoEvents(body, signal, (event) => {
          announcePhotoEvent(event);
          mark(event);
        });
      });
    } catch (cause) {
      if (!current.signal.aborted) {
        failure(cause);
        if (cause instanceof APIError && [401, 403].includes(cause.status)) return dispose();
      }
    }
    if (!stopped && !current.signal.aborted)
      retry = setTimeout(() => void connect(), Math.min(30_000, 1000 * 2 ** connections++));
  };
  // Missed changes are not replayed, so every reopened stream starts with a refetch.
  const open = (refetch: boolean) => {
    controller.abort();
    controller = new AbortController();
    clearTimeout(retry);
    if (stopped || document.visibilityState !== "visible") return;
    if (refetch) mark();
    void connect();
  };
  // Also the way to reopen the stream when the Hidden unlock changes, since the server binds it at connect.
  const reconnect = () => open(true);
  const unlisten = onPhotoEventSignal((event) => mark(parsePhotoEvent(event)));
  const dispose = () => {
    stopped = true;
    controller.abort();
    clearTimeout(pass);
    clearTimeout(retry);
    unlisten();
    document.removeEventListener("visibilitychange", reconnect);
  };
  document.addEventListener("visibilitychange", reconnect);
  // Followers can miss changes before their listener exists, even while another tab owns the stream.
  open(true);
  return { stop: dispose, reconnect };
}
