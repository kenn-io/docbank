import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { APIError } from "./api-transport.js";
import {
  photoEventIdleTimeout,
  readPhotoEvents,
  type PhotoEventBatch,
  watchPhotoEvents,
} from "./photoEvents.js";

const encoder = new TextEncoder();
function stream(chunks: string[]) {
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

beforeEach(() => {
  const value = {
    request: async (_name: string, _options: unknown, run: (lock: Lock) => unknown) =>
      run({ name: "docbank-photo-events" } as Lock),
  };
  vi.stubGlobal("navigator", { locks: value });
});

it("parses event frames, rejects invalid data and cancels reads", async () => {
  const change = vi.fn();
  await readPhotoEvents(
    stream([
      ": keepalive\r",
      "\n\r\nevent: cha",
      'nge\r\ndata: {"asset_id":"photo-a",\r\n',
      'data: "revision":4}\r',
      "\n\r",
      '\ndata: {"refetch":true}\n\n',
    ]),
    new AbortController().signal,
    change,
  );
  expect(change.mock.calls).toEqual([[{ asset_id: "photo-a", revision: 4 }], [{ refetch: true }]]);
  await expect(
    readPhotoEvents(stream([`data: ${"x".repeat(65537)}`]), new AbortController().signal, vi.fn()),
  ).rejects.toThrow("frame limit");
  await expect(
    readPhotoEvents(stream(["data: nope\n\n"]), new AbortController().signal, vi.fn()),
  ).rejects.toThrow(SyntaxError);
  const cancelled = vi.fn();
  const body = new ReadableStream<Uint8Array>({ cancel: cancelled });
  const controller = new AbortController();
  const reading = readPhotoEvents(body, controller.signal, vi.fn());
  controller.abort();
  await reading;
  expect(cancelled).toHaveBeenCalledOnce();
});

it("authenticates and coalesces changes while preserving a change during refresh", async () => {
  vi.useFakeTimers();
  let writer!: ReadableStreamDefaultController<Uint8Array>;
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      writer = controller;
    },
  });
  const fetch = vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(new Response(body, { headers: { "Content-Type": "text/event-stream" } }));
  let release!: () => void;
  const refresh = vi
    .fn()
    .mockImplementation(() => new Promise<void>((resolve) => (release = resolve)));
  const stop = watchPhotoEvents("tab-secret", refresh, vi.fn());
  await vi.advanceTimersByTimeAsync(0);
  expect(fetch.mock.calls[0][0]).toBe("/api/v1/photos/events");
  expect(new Headers(fetch.mock.calls[0][1]?.headers).get("X-Docbank-Web-Session")).toBe(
    "tab-secret",
  );
  for (const [index, change] of [
    { asset_id: "a", revision: 1 },
    { asset_id: "a", revision: 4 },
    { asset_id: "a", revision: 2 },
    { album_id: "b", revision: 3 },
  ].entries()) {
    writer.enqueue(encoder.encode(`data: ${JSON.stringify(change)}\n\n`));
    await vi.advanceTimersByTimeAsync(index === 3 ? 24 : 25);
    expect(refresh).not.toHaveBeenCalled();
  }
  await vi.advanceTimersByTimeAsync(1);
  expect(refresh).toHaveBeenCalledTimes(1);
  expect(refresh.mock.calls[0][0]).toEqual({
    assets: new Map([["a", 4]]),
    albums: new Map([["b", 3]]),
    refetch: false,
  });
  writer.enqueue(encoder.encode('data: {"asset_id":"c","revision":2}\n\n'));
  await vi.advanceTimersByTimeAsync(100);
  expect(refresh).toHaveBeenCalledTimes(1);
  release();
  await vi.advanceTimersByTimeAsync(999);
  expect(refresh).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(1);
  expect(refresh).toHaveBeenCalledTimes(2);
  expect(refresh.mock.calls[1][0]).toEqual({
    assets: new Map([["c", 2]]),
    albums: new Map(),
    refetch: false,
  });
  writer.enqueue(
    encoder.encode('data: {"asset_id":"d","revision":1}\n\ndata: {"refetch":true}\n\n'),
  );
  await vi.advanceTimersByTimeAsync(100);
  expect(refresh).toHaveBeenCalledTimes(2);
  release();
  await vi.advanceTimersByTimeAsync(1000);
  expect(refresh).toHaveBeenCalledTimes(3);
  expect(refresh.mock.calls[2][0]).toEqual({ assets: new Map(), albums: new Map(), refetch: true });
  release();
  stop();
});

it("backs off failed stream connections, recovers and stops on expired authentication", async () => {
  vi.useFakeTimers();
  const fetch = vi.spyOn(globalThis, "fetch").mockRejectedValue(new Error("offline"));
  const failure = vi.fn(),
    connected = vi.fn();
  const stop = watchPhotoEvents("tab-secret", vi.fn(), failure, connected);
  await vi.advanceTimersByTimeAsync(1000);
  expect(fetch).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(1999);
  expect(fetch).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(1);
  expect(fetch).toHaveBeenCalledTimes(3);
  fetch.mockResolvedValueOnce(
    Response.json(
      { detail: "Close another photo view and retry.", code: "photo_event_stream_limit" },
      { status: 429 },
    ),
  );
  await vi.advanceTimersByTimeAsync(4000);
  expect(fetch).toHaveBeenCalledTimes(4);
  expect(failure).toHaveBeenCalledWith(
    expect.objectContaining({ status: 429, message: "Close another photo view and retry." }),
  );
  expect(connected).not.toHaveBeenCalled();
  fetch.mockResolvedValueOnce(
    new Response(stream([]), { headers: { "Content-Type": "text/event-stream" } }),
  );
  await vi.advanceTimersByTimeAsync(8000);
  expect(fetch).toHaveBeenCalledTimes(5);
  expect(connected).toHaveBeenCalledOnce();
  fetch.mockResolvedValue(new Response(JSON.stringify({ detail: "expired" }), { status: 401 }));
  await vi.advanceTimersByTimeAsync(16000);
  expect(fetch).toHaveBeenCalledTimes(6);
  expect(failure).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(60_000);
  expect(fetch).toHaveBeenCalledTimes(6);
  stop();
});

async function failedRefreshWatcher() {
  vi.useFakeTimers();
  const visible = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  let writer!: ReadableStreamDefaultController<Uint8Array>;
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(
    async () =>
      new Response(
        new ReadableStream<Uint8Array>({
          start(c) {
            writer = c;
          },
        }),
        { headers: { "Content-Type": "text/event-stream" } },
      ),
  );
  let reject!: (cause: Error) => void;
  const refresh = vi
    .fn()
    .mockImplementationOnce(() => new Promise<void>((_, fail) => (reject = fail)))
    .mockRejectedValue(new Error("offline"));
  const failure = vi.fn(),
    recovered = vi.fn();
  const stop = watchPhotoEvents("synthetic", refresh, failure, vi.fn(), recovered);
  const send = (id: string) => writer.enqueue(encoder.encode(`data: {"asset_id":"${id}"}\n\n`));
  await vi.advanceTimersByTimeAsync(0);
  send("first");
  await vi.advanceTimersByTimeAsync(100);
  reject(new APIError("Hidden locked", 403, "hidden_locked"));
  await vi.advanceTimersByTimeAsync(0);
  expect(fetch).toHaveBeenCalledOnce();
  expect(failure).toHaveBeenCalledOnce();
  return { visible, fetch, refresh, failure, recovered, stop, send };
}

it("retries failed refreshes with backoff and resets the delay after recovery", async () => {
  const { refresh, recovered, send, stop } = await failedRefreshWatcher();
  expect(recovered).not.toHaveBeenCalled();
  for (const delay of [1000, 2000, 4000, 8000, 16000, 30000, 30000]) {
    const calls = refresh.mock.calls.length;
    await vi.advanceTimersByTimeAsync(delay - 1);
    expect(refresh).toHaveBeenCalledTimes(calls);
    await vi.advanceTimersByTimeAsync(1);
    expect(refresh).toHaveBeenCalledTimes(calls + 1);
    expect(refresh.mock.lastCall?.[0]).toEqual({
      assets: new Map(),
      albums: new Map(),
      refetch: true,
    });
  }
  refresh.mockResolvedValue(undefined);
  await vi.advanceTimersByTimeAsync(30000);
  expect(recovered).toHaveBeenCalledOnce();
  refresh.mockRejectedValueOnce(new Error("again"));
  send("new");
  await vi.advanceTimersByTimeAsync(1000);
  const calls = refresh.mock.calls.length;
  await vi.advanceTimersByTimeAsync(999);
  expect(refresh).toHaveBeenCalledTimes(calls);
  await vi.advanceTimersByTimeAsync(1);
  expect(refresh).toHaveBeenCalledTimes(calls + 1);
  stop();
});

it.each(["pause", "dispose"])("aborts a failed-refresh stream on %s", async (action) => {
  const { visible, fetch, refresh, stop } = await failedRefreshWatcher();
  const signal = fetch.mock.calls[0][1]!.signal!;
  expect(signal.aborted).toBe(false);
  if (action === "dispose") stop();
  else {
    visible.mockReturnValue("hidden");
    document.dispatchEvent(new Event("visibilitychange"));
  }
  expect(signal.aborted).toBe(true);
  await vi.advanceTimersByTimeAsync(60000);
  expect(refresh).toHaveBeenCalledOnce();
  expect(fetch).toHaveBeenCalledOnce();
  if (action === "pause") {
    refresh.mockResolvedValue(undefined);
    visible.mockReturnValue("visible");
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(100);
    expect(refresh).toHaveBeenCalledTimes(2);
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(refresh.mock.lastCall?.[0]).toEqual({
      assets: new Map(),
      albums: new Map(),
      refetch: true,
    });
  }
  stop();
});

it("ignores refresh completion after the watcher is disposed", async () => {
  vi.useFakeTimers();
  let writer!: ReadableStreamDefaultController<Uint8Array>;
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      new ReadableStream<Uint8Array>({
        start(c) {
          writer = c;
        },
      }),
      { headers: { "Content-Type": "text/event-stream" } },
    ),
  );
  let finish!: () => void;
  const recovered = vi.fn();
  const stop = watchPhotoEvents(
    "synthetic",
    () => new Promise<void>((resolve) => (finish = resolve)),
    vi.fn(),
    vi.fn(),
    recovered,
  );
  await vi.advanceTimersByTimeAsync(0);
  writer.enqueue(encoder.encode('data: {"refetch":true}\n\n'));
  await vi.advanceTimersByTimeAsync(100);
  stop();
  finish();
  await vi.advanceTimersByTimeAsync(0);
  expect(recovered).not.toHaveBeenCalled();
});

it("reconnects a stream that stops sending keepalives", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  const silent = () =>
    new Response(new ReadableStream<Uint8Array>({}), {
      headers: { "Content-Type": "text/event-stream" },
    });
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => silent());
  const stop = watchPhotoEvents("synthetic", vi.fn(), vi.fn());
  await vi.advanceTimersByTimeAsync(photoEventIdleTimeout - 1);
  expect(fetch).toHaveBeenCalledOnce();
  await vi.advanceTimersByTimeAsync(1);
  await vi.advanceTimersByTimeAsync(1000);
  expect(fetch).toHaveBeenCalledTimes(2);
  stop();
});

it("keeps reading through keepalives and cancels after the next idle deadline", async () => {
  vi.useFakeTimers();
  let writer!: ReadableStreamDefaultController<Uint8Array>;
  const cancelled = vi.fn();
  const body = new ReadableStream<Uint8Array>({
    cancel: cancelled,
    start(controller) {
      writer = controller;
    },
  });
  const controller = new AbortController();
  const reading = readPhotoEvents(body, controller.signal, vi.fn(), 1000);
  for (let beat = 0; beat < 5; beat++) {
    await vi.advanceTimersByTimeAsync(900);
    writer.enqueue(encoder.encode(": keepalive\n\n"));
  }
  await vi.advanceTimersByTimeAsync(999);
  expect(cancelled).not.toHaveBeenCalled();
  const failed = expect(reading).rejects.toThrow();
  await vi.advanceTimersByTimeAsync(1);
  await failed;
  expect(cancelled).toHaveBeenCalledOnce();
});

it("reconnects a stream whose response headers never arrive", async () => {
  vi.useFakeTimers();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  const signals: AbortSignal[] = [];
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(
    (_url, init) =>
      new Promise<Response>((_, reject) => {
        const signal = init!.signal!;
        signals.push(signal);
        signal.addEventListener("abort", () => reject(signal.reason), { once: true });
      }),
  );
  const stop = watchPhotoEvents("synthetic", vi.fn(), vi.fn());
  await vi.advanceTimersByTimeAsync(photoEventIdleTimeout - 1);
  expect(signals[0]?.aborted).toBe(false);
  await vi.advanceTimersByTimeAsync(1);
  expect(signals[0]?.aborted).toBe(true);
  await vi.advanceTimersByTimeAsync(1000);
  expect(fetch).toHaveBeenCalledTimes(2);
  stop();
});

it("broadcasts one ID-free pulse from the holder's parsed change", async () => {
  vi.useFakeTimers();
  let writer!: ReadableStreamDefaultController<Uint8Array>;
  const postMessage = vi.fn();
  const close = vi.fn();
  vi.stubGlobal(
    "BroadcastChannel",
    class {
      postMessage = postMessage;
      close = close;
    },
  );
  vi.stubGlobal("navigator", {
    locks: {
      request: vi.fn(async (_name, _options, run) => run({ name: "docbank-photo-events" })),
    },
  });
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      new ReadableStream({
        start(c) {
          writer = c;
        },
      }),
      { headers: { "Content-Type": "text/event-stream" } },
    ),
  );
  const refresh = vi.fn();
  const stop = watchPhotoEvents("holder", refresh, vi.fn());
  await vi.advanceTimersByTimeAsync(0);
  writer.enqueue(encoder.encode('data: {"asset_id":"private-photo","revision":7}\n\n'));
  await vi.advanceTimersByTimeAsync(100);
  expect(postMessage).toHaveBeenCalledExactlyOnceWith("refetch");
  expect(refresh).toHaveBeenCalledExactlyOnceWith({
    assets: new Map([["private-photo", 7]]),
    albums: new Map(),
    refetch: false,
  });
  stop();
  await vi.advanceTimersByTimeAsync(0);
  expect(close).toHaveBeenCalledOnce();
});

it("refreshes a Web Locks follower through ID-free pulses without opening a stream", async () => {
  vi.useFakeTimers();
  let channel!: { onmessage?: (event: MessageEvent<unknown>) => void };
  const close = vi.fn();
  vi.stubGlobal(
    "BroadcastChannel",
    class {
      onmessage?: (event: MessageEvent<unknown>) => void;
      close = close;
      constructor() {
        channel = this;
      }
    },
  );
  vi.stubGlobal("navigator", {
    locks: {
      request: vi.fn(async (_name, options, run) => {
        if (options.ifAvailable) return run(null);
        await new Promise<void>((resolve) =>
          options.signal.addEventListener("abort", () => resolve(), { once: true }),
        );
      }),
    },
  });
  const fetch = vi.spyOn(globalThis, "fetch");
  const refresh = vi.fn(async (_batch: PhotoEventBatch) => {});
  const stop = watchPhotoEvents("follower", refresh, vi.fn());
  await vi.advanceTimersByTimeAsync(100);
  expect(refresh).toHaveBeenCalledExactlyOnceWith({
    assets: new Map(),
    albums: new Map(),
    refetch: true,
  });
  channel.onmessage?.({ data: "refetch" } as MessageEvent<unknown>);
  await vi.advanceTimersByTimeAsync(1000);
  expect(refresh).toHaveBeenCalledTimes(2);
  expect(refresh.mock.lastCall?.[0]).toEqual({
    assets: new Map(),
    albums: new Map(),
    refetch: true,
  });
  expect(fetch).not.toHaveBeenCalled();
  stop();
  await vi.advanceTimersByTimeAsync(0);
  expect(close).toHaveBeenCalledOnce();
});

it("refreshes six visible tabs without streams when Web Locks are unavailable", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("navigator", {});
  const fetch = vi.spyOn(globalThis, "fetch");
  let finish!: () => void;
  const refreshes = Array.from({ length: 6 }, () => vi.fn(async () => {}));
  refreshes[0]!.mockImplementationOnce(() => new Promise<void>((resolve) => (finish = resolve)));
  const stops = refreshes.map((refresh, index) =>
    watchPhotoEvents(`tab-${index}`, refresh, vi.fn()),
  );
  await vi.advanceTimersByTimeAsync(100);
  for (const refresh of refreshes)
    expect(refresh).toHaveBeenCalledExactlyOnceWith({
      assets: new Map(),
      albums: new Map(),
      refetch: true,
    });
  await vi.advanceTimersByTimeAsync(2000);
  expect(refreshes[0]).toHaveBeenCalledOnce();
  for (const refresh of refreshes.slice(1)) expect(refresh).toHaveBeenCalledTimes(3);
  finish();
  await vi.advanceTimersByTimeAsync(999);
  expect(refreshes[0]).toHaveBeenCalledOnce();
  await vi.advanceTimersByTimeAsync(1);
  expect(refreshes[0]).toHaveBeenCalledTimes(2);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  document.dispatchEvent(new Event("visibilitychange"));
  const counts = refreshes.map((refresh) => refresh.mock.calls.length);
  await vi.advanceTimersByTimeAsync(5000);
  expect(refreshes.map((refresh) => refresh.mock.calls.length)).toEqual(counts);
  expect(fetch).not.toHaveBeenCalled();
  for (const stop of stops) stop();
});
