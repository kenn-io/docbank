import { afterEach, expect, it, vi } from "vite-plus/test";
import { readPhotoEvents, watchPhotoEvents } from "./photoEvents.js";
import type { PhotoChange } from "./photoEvents.js";
import { idFreeChange, photoChange } from "./photo-test-fixtures.js";

const encoder = new TextEncoder();
function stream(chunks: string[]) {
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
}
// Serves one open stream per request and hands the caller a way to write to the latest.
function serveStream() {
  const stream = { write: (_frame: string) => {}, cancel: vi.fn() };
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(
    async () =>
      new Response(
        new ReadableStream({
          start(controller) {
            stream.write = (frame) => controller.enqueue(encoder.encode(frame));
          },
          cancel: stream.cancel,
        }),
      ),
  );
  return { stream, fetch };
}
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
  Reflect.deleteProperty(navigator, "locks");
});

it("reconciles a returning follower while the owner keeps the stream", async () => {
  vi.useFakeTimers();
  const { fetch } = serveStream();
  Object.defineProperty(navigator, "locks", {
    configurable: true,
    value: {
      request: vi.fn(
        (_name: string, { signal }: { signal: AbortSignal }) =>
          new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve())),
      ),
    },
  });
  const refresh = vi.fn((_change: PhotoChange) => Promise.resolve());
  const follower = watchPhotoEvents("synthetic", refresh, vi.fn());
  await vi.advanceTimersByTimeAsync(0);
  expect(refresh).toHaveBeenCalledExactlyOnceWith(idFreeChange);
  expect(fetch).not.toHaveBeenCalled();
  follower.stop();
});

it("recognizes change frames, bounds incomplete frames, and cancels reads", async () => {
  const received = vi.fn();
  await readPhotoEvents(
    stream([
      ": keepalive\r",
      "\n\r\nevent: cha",
      'nge\r\ndata: {"asset_id":\r\n',
      'data: "photo-a"}\r',
      "\n\r",
      "\ndata: {}\n\n",
    ]),
    new AbortController().signal,
    received,
  );
  expect(received.mock.calls).toEqual([[{ asset_id: "photo-a" }], [{}]]);
  await expect(
    readPhotoEvents(stream([`data: ${"x".repeat(65537)}`]), new AbortController().signal, vi.fn()),
  ).rejects.toThrow("frame limit");
  const cancelled = vi.fn();
  const body = new ReadableStream<Uint8Array>({ cancel: cancelled });
  const controller = new AbortController();
  const reading = readPhotoEvents(body, controller.signal, vi.fn());
  controller.abort();
  await reading;
  expect(cancelled).toHaveBeenCalledOnce();
});

it("authenticates, names the changed photos, and queues exactly one more pass for events during a pass", async () => {
  vi.useFakeTimers();
  const { stream, fetch } = serveStream();
  let release!: () => void;
  const refresh = vi.fn((_change: PhotoChange) => Promise.resolve());
  const watcher = watchPhotoEvents("synthetic", refresh, vi.fn());
  await vi.advanceTimersByTimeAsync(0);
  expect(new Headers(fetch.mock.calls[0][1]?.headers).get("X-Docbank-Web-Session")).toBe(
    "synthetic",
  );
  // Connecting asks for a refetch, since a change can land before the server subscribes.
  expect(refresh).toHaveBeenCalledTimes(1);
  expect(refresh).toHaveBeenLastCalledWith(idFreeChange);
  refresh.mockImplementationOnce(() => new Promise<void>((resolve) => (release = resolve)));
  stream.write('data: {"asset_id":"open"}\n\n');
  await vi.advanceTimersByTimeAsync(1000);
  expect(refresh).toHaveBeenLastCalledWith(photoChange("open"));
  expect(refresh).toHaveBeenCalledTimes(2);
  for (const id of ["later", "later", "last"]) stream.write(`data: {"asset_id":"${id}"}\n\n`);
  await vi.advanceTimersByTimeAsync(5000);
  expect(refresh).toHaveBeenCalledTimes(2);
  release();
  // The held pass took 5 s, so the next waits 10 s.
  await vi.advanceTimersByTimeAsync(9000);
  expect(refresh).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(1000);
  expect(refresh).toHaveBeenCalledTimes(3);
  expect(refresh).toHaveBeenLastCalledWith(photoChange("later", "last"));
  await vi.advanceTimersByTimeAsync(10_000);
  expect(refresh).toHaveBeenCalledTimes(3);
  // Reconnecting, as after a Hidden unlock change, swaps the stream and refetches.
  watcher.reconnect();
  await vi.advanceTimersByTimeAsync(1000);
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(stream.cancel).toHaveBeenCalledOnce();
  expect(refresh).toHaveBeenCalledTimes(4);
  watcher.stop();
  await vi.advanceTimersByTimeAsync(0);
  expect(stream.cancel).toHaveBeenCalledTimes(2);
});

it("forwards the owner's changed photo to followers and takes it from the owner's channel", async () => {
  vi.resetModules();
  const { watchPhotoEvents } = await import("./photoEvents.js");
  vi.useFakeTimers();
  const posted: unknown[] = [];
  let hear!: (data: unknown) => void;
  vi.stubGlobal(
    "BroadcastChannel",
    class {
      postMessage(data: unknown) {
        posted.push(data);
      }
      addEventListener(_type: string, listener: (event: MessageEvent) => void) {
        hear = (data) => listener({ data } as MessageEvent);
      }
      removeEventListener() {}
    },
  );
  const { stream } = serveStream();
  const refresh = vi.fn((_change: PhotoChange) => Promise.resolve());
  const owner = watchPhotoEvents("synthetic", refresh, vi.fn());
  await vi.advanceTimersByTimeAsync(0);
  stream.write('data: {"asset_id":"photo-a"}\n\n');
  await vi.advanceTimersByTimeAsync(0);
  expect(posted.at(-1)).toEqual({ asset_id: "photo-a" });
  await vi.advanceTimersByTimeAsync(1000);
  refresh.mockClear();
  hear({ asset_id: "photo-b" });
  await vi.advanceTimersByTimeAsync(1000);
  expect(refresh).toHaveBeenLastCalledWith(photoChange("photo-b"));
  hear({});
  await vi.advanceTimersByTimeAsync(1000);
  expect(refresh).toHaveBeenLastCalledWith(idFreeChange);
  owner.stop();
});

it("reports each time the stream opens, including after a failed connection", async () => {
  vi.useFakeTimers();
  serveStream();
  const fetch = vi.mocked(globalThis.fetch);
  fetch.mockRejectedValueOnce(new TypeError("Failed to fetch"));
  const failure = vi.fn(),
    opened = vi.fn();
  const watcher = watchPhotoEvents(
    "synthetic",
    vi.fn().mockResolvedValue(undefined),
    failure,
    opened,
  );
  await vi.advanceTimersByTimeAsync(0);
  expect(failure).toHaveBeenCalledOnce();
  expect(opened).not.toHaveBeenCalled();
  await vi.advanceTimersByTimeAsync(1000);
  expect(opened).toHaveBeenCalledOnce();
  watcher.stop();
});

it("lets a pass that only retries a failed one resume, but not one with new events", async () => {
  vi.useFakeTimers();
  const { stream } = serveStream();
  const refresh = vi
    .fn<(change: PhotoChange) => Promise<boolean | void>>()
    .mockResolvedValueOnce(undefined)
    .mockRejectedValueOnce(new Error("Page two failed"))
    .mockRejectedValueOnce(new Error("Page two failed"))
    .mockResolvedValue(undefined);
  const watcher = watchPhotoEvents("synthetic", refresh, vi.fn());
  await vi.advanceTimersByTimeAsync(0);
  expect(refresh.mock.calls[0]![0].resume).toBeUndefined();
  stream.write('data: {"asset_id":"a"}\n\n');
  await vi.advanceTimersByTimeAsync(1000);
  expect(refresh).toHaveBeenCalledTimes(2);
  // Nothing new arrived while recovering, so the retry keeps what the failed pass staged.
  await vi.advanceTimersByTimeAsync(1000);
  expect(refresh).toHaveBeenCalledTimes(3);
  expect(refresh.mock.calls[2]![0]).toMatchObject({ resume: true, ids: new Set(["a"]) });
  // A new event while the retry fails means the staged pages may be stale.
  stream.write('data: {"asset_id":"b"}\n\n');
  await vi.advanceTimersByTimeAsync(5000);
  expect(refresh).toHaveBeenCalledTimes(4);
  expect(refresh.mock.calls[3]![0].resume).toBeUndefined();
  expect(refresh.mock.calls[3]![0].ids).toEqual(new Set(["a", "b"]));
  watcher.stop();
});

it("retries a failed pass with backoff and a pass that asks to wait without a failure", async () => {
  vi.useFakeTimers();
  serveStream();
  const refresh = vi
    .fn<(change: PhotoChange) => Promise<boolean | void>>()
    .mockResolvedValueOnce(false)
    .mockRejectedValueOnce(new Error("Busy"))
    .mockRejectedValueOnce(new Error("Busy"))
    .mockResolvedValue(undefined);
  const failure = vi.fn();
  const watcher = watchPhotoEvents("synthetic", refresh, failure);
  await vi.advanceTimersByTimeAsync(0);
  expect(refresh).toHaveBeenCalledTimes(1);
  // A pass that asks to wait comes back after the usual second.
  await vi.advanceTimersByTimeAsync(999);
  expect(refresh).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(1);
  expect(refresh).toHaveBeenCalledTimes(2);
  // Failures back off: one second, then two.
  await vi.advanceTimersByTimeAsync(999);
  expect(refresh).toHaveBeenCalledTimes(2);
  await vi.advanceTimersByTimeAsync(1);
  expect(refresh).toHaveBeenCalledTimes(3);
  await vi.advanceTimersByTimeAsync(1999);
  expect(refresh).toHaveBeenCalledTimes(3);
  await vi.advanceTimersByTimeAsync(1);
  expect(refresh).toHaveBeenCalledTimes(4);
  await vi.advanceTimersByTimeAsync(30_000);
  expect(refresh).toHaveBeenCalledTimes(4);
  expect(failure).not.toHaveBeenCalled();
  watcher.stop();
});

it("asks for a full refetch when a pass would name more than 64 photos", async () => {
  vi.useFakeTimers();
  const { stream } = serveStream();
  const refresh = vi.fn((_change: PhotoChange) => Promise.resolve());
  const watcher = watchPhotoEvents("synthetic", refresh, vi.fn());
  await vi.advanceTimersByTimeAsync(0);
  for (let i = 0; i < 65; i++) stream.write(`data: {"asset_id":"photo-${i}"}\n\n`);
  await vi.advanceTimersByTimeAsync(1000);
  expect(refresh.mock.calls[1][0]).toEqual(idFreeChange);
  watcher.stop();
});
