import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { APIError } from "./api-transport.js";
import { readPhotoEvents, type PhotoEventBatch, watchPhotoEvents } from "./photoEvents.js";

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
    { asset_id: "b", revision: 3 },
  ].entries()) {
    writer.enqueue(encoder.encode(`data: ${JSON.stringify(change)}\n\n`));
    await vi.advanceTimersByTimeAsync(index === 3 ? 24 : 25);
    expect(refresh).not.toHaveBeenCalled();
  }
  await vi.advanceTimersByTimeAsync(1);
  expect(refresh).toHaveBeenCalledTimes(1);
  expect(refresh.mock.calls[0][0]).toEqual({
    assets: new Map([
      ["a", 4],
      ["b", 3],
    ]),
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
  expect(refresh.mock.calls[2][0]).toEqual({ assets: new Map(), refetch: true });
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

it("uses cursor polling when locks are unavailable", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("navigator", {});
  const fetch = vi.spyOn(globalThis, "fetch");
  const refresh = vi.fn().mockResolvedValue(undefined);
  let receive!: (cursor?: string) => void;
  const stop = watchPhotoEvents("synthetic", refresh, vi.fn(), vi.fn(), vi.fn(), (poll) => {
    receive = poll;
  });
  receive("first");
  await vi.advanceTimersByTimeAsync(100);
  expect(refresh).toHaveBeenCalledOnce();
  for (let i = 0; i < 4; i++) {
    receive("first");
    await vi.advanceTimersByTimeAsync(2000);
  }
  expect(refresh).toHaveBeenCalledOnce();
  expect(fetch).not.toHaveBeenCalled();
  receive("second");
  await vi.advanceTimersByTimeAsync(100);
  expect(refresh).toHaveBeenCalledTimes(2);
  stop();
});

it("limits 1,000 events paced over one minute to 60 refresh batches", async () => {
  vi.useFakeTimers();
  let writer!: ReadableStreamDefaultController<Uint8Array>;
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      new ReadableStream<Uint8Array>({
        start(controller) {
          writer = controller;
        },
      }),
      { headers: { "Content-Type": "text/event-stream" } },
    ),
  );
  const times: number[] = [];
  const batches: PhotoEventBatch[] = [];
  const refresh = vi.fn(async (batch: PhotoEventBatch) => {
    times.push(Date.now());
    batches.push(batch);
  });
  const stop = watchPhotoEvents("synthetic", refresh, vi.fn());
  try {
    await vi.advanceTimersByTimeAsync(0);
    for (let i = 0; i < 1000; i++) {
      writer.enqueue(encoder.encode(`data: {"asset_id":"photo-${i}","revision":1}\n\n`));
      await vi.advanceTimersByTimeAsync(60);
    }
    expect(refresh).toHaveBeenCalledTimes(60);
    await vi.advanceTimersByTimeAsync(1000);
    expect(refresh).toHaveBeenCalledTimes(61);
    expect(batches.reduce((total, batch) => total + batch.assets.size, 0)).toBe(1000);
    expect(times.slice(1).every((time, index) => time - times[index] >= 1000)).toBe(true);
  } finally {
    stop();
  }
});

it("uses each follower's cursor while waiting for ownership and refetches on handoff", async () => {
  vi.useFakeTimers();
  let acquire!: () => Promise<unknown>;
  vi.stubGlobal("navigator", {
    locks: {
      request: async (
        _name: string,
        options: { ifAvailable?: boolean; signal?: AbortSignal },
        run: (lock: Lock | null) => unknown,
      ) => {
        if (options.ifAvailable) return run(null);
        acquire = async () => run({ name: "docbank-photo-events" } as Lock);
        return new Promise<void>((resolve) =>
          options.signal?.addEventListener("abort", () => resolve(), { once: true }),
        );
      },
    },
  });
  vi.stubGlobal("BroadcastChannel", undefined);
  const fetcher = vi
    .spyOn(globalThis, "fetch")
    .mockImplementation(
      async () =>
        new Response(new ReadableStream(), { headers: { "Content-Type": "text/event-stream" } }),
    );
  const refresh = vi.fn().mockResolvedValue(undefined);
  let receive!: (cursor?: string) => void;
  const stop = watchPhotoEvents("follower", refresh, vi.fn(), vi.fn(), vi.fn(), (poll) => {
    receive = poll;
  });
  try {
    await vi.advanceTimersByTimeAsync(0);
    receive("locked");
    await vi.advanceTimersByTimeAsync(100);
    expect(refresh).toHaveBeenCalledExactlyOnceWith({ assets: new Map(), refetch: true });
    receive("unlocked-hidden-change");
    await vi.advanceTimersByTimeAsync(1000);
    expect(refresh).toHaveBeenCalledTimes(2);
    expect(fetcher).not.toHaveBeenCalled();
    void acquire();
    await vi.advanceTimersByTimeAsync(1000);
    expect(fetcher).toHaveBeenCalledOnce();
    expect(refresh).toHaveBeenCalledTimes(3);
    receive("owner-cursor");
    await vi.advanceTimersByTimeAsync(1000);
    expect(refresh).toHaveBeenCalledTimes(3);
  } finally {
    stop();
  }
});

it("uses access cursors during failed connections and reconnect backoff", async () => {
  vi.useFakeTimers();
  vi.spyOn(globalThis, "fetch").mockRejectedValue(new Error("offline"));
  const refresh = vi.fn().mockResolvedValue(undefined);
  let receive!: (cursor?: string) => void;
  const stop = watchPhotoEvents("tab", refresh, vi.fn(), vi.fn(), vi.fn(), (poll) => {
    receive = poll;
  });
  try {
    await vi.advanceTimersByTimeAsync(0);
    receive("first");
    await vi.advanceTimersByTimeAsync(100);
    receive("second");
    await vi.advanceTimersByTimeAsync(1000);
    expect(refresh).toHaveBeenCalledTimes(2);
  } finally {
    stop();
  }
});
