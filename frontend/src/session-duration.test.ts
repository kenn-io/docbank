import { afterEach, beforeEach, describe, expect, it, type MockInstance, vi } from "vitest";
import { startSessionReporting } from "./session-duration.js";

describe("session_ended reporting", () => {
  let stop: (() => Promise<void>) | undefined;
  let now: number;
  let hidden: boolean;
  let fetchMock: MockInstance<typeof fetch>;

  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
    now = 0;
    hidden = false;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
    vi.setSystemTime(new Date("2026-03-04T10:00:00Z"));
    fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(
      async () => new Response('{"status":"queued"}', { status: 202 }),
    );
  });

  afterEach(() => {
    void stop?.();
    stop = undefined;
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  const sessionReports = () =>
    fetchMock.mock.calls.filter(([, init]) => JSON.parse(String(init?.body)).event === "session_ended");
  const expectBucket = (bucket: string) => {
    expect(sessionReports()).toHaveLength(1);
    const [url, init] = sessionReports()[0] ?? [];
    expect(url).toBe("/api/daemon/telemetry/events");
    expect(init?.keepalive).toBe(true);
    expect(new Headers(init?.headers).get("X-Docbank-Web-Session")).toBe("secret");
    expect(JSON.parse(String(init?.body))).toEqual({
      event: "session_ended",
      properties: { surface: "web", duration_bucket: bucket },
    });
  };
  const visibility = (value: boolean) => {
    hidden = value;
    document.dispatchEvent(new Event("visibilitychange"));
  };

  it("reports nothing for a hidden-only page", async () => {
    hidden = true;
    stop = startSessionReporting("secret");
    now = 3_600_000;
    await vi.advanceTimersByTimeAsync(3_600_000);
    window.dispatchEvent(new Event("pagehide"));
    await stop();
    expect(sessionReports()).toHaveLength(0);
  });

  it("sums twenty one-minute visible stretches into one session", async () => {
    stop = startSessionReporting("secret");
    for (let i = 0; i < 20; i++) {
      now += 60_000;
      visibility(true);
      now += 600_000;
      await vi.advanceTimersByTimeAsync(600_000);
      expect(sessionReports()).toHaveLength(0);
      visibility(false);
    }
    window.dispatchEvent(new Event("pagehide"));
    await stop();
    expectBucket("5_to_30m");
  });

  it("ends the session after thirty hidden minutes without a second pagehide report", async () => {
    stop = startSessionReporting("secret");
    now = 120_000;
    visibility(true);
    now += 1_800_000;
    await vi.advanceTimersByTimeAsync(1_799_999);
    expect(sessionReports()).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1);
    expectBucket("1_to_5m");
    window.dispatchEvent(new Event("pagehide"));
    await stop();
    expectBucket("1_to_5m");
  });

  it.each([
    [1, "under_1m"], [59_999, "under_1m"], [60_000, "1_to_5m"],
    [299_999, "1_to_5m"], [300_000, "5_to_30m"],
    [1_800_000, "5_to_30m"], [1_800_001, "over_30m"],
  ])("buckets %i visible milliseconds as %s", async (elapsed, bucket) => {
    stop = startSessionReporting("secret");
    now = Number(elapsed);
    window.dispatchEvent(new Event("pagehide"));
    await stop();
    expectBucket(String(bucket));
  });

  it.each([
    ["bfcache restore", 30_000, "under_1m"],
    ["visibilitychange with a suspended timer", 240_000, "1_to_5m"],
    ["pageshow with a suspended timer", 240_000, "1_to_5m"],
  ])("starts a fresh session on %s", async (resume, elapsed, bucket) => {
    stop = startSessionReporting("secret");
    now = 120_000;
    if (resume === "bfcache restore") window.dispatchEvent(new Event("pagehide"));
    else visibility(true);
    now += 3_600_000;
    vi.setSystemTime(Date.now() + 3_600_000);
    if (resume === "visibilitychange with a suspended timer") visibility(false);
    else {
      hidden = false;
      window.dispatchEvent(new Event("pageshow"));
    }
    now += Number(elapsed);
    window.dispatchEvent(new Event("pagehide"));
    await stop();
    expect(
      sessionReports().map(([, init]) => JSON.parse(String(init?.body)).properties.duration_bucket),
    ).toEqual(["1_to_5m", bucket]);
  });

  it.each([false, true])("stop waits for the pending send and never rejects, failure=%s", async (failure) => {
    stop = startSessionReporting("secret");
    let resolve!: (response: Response) => void;
    let reject!: (cause: Error) => void;
    fetchMock.mockImplementationOnce(() => new Promise<Response>((yes, no) => { resolve = yes; reject = no; }));
    now = 120_000;
    visibility(true);
    const stopped = vi.fn();
    const pending = stop().then(stopped);
    expectBucket("1_to_5m");
    await vi.advanceTimersByTimeAsync(0);
    expect(stopped).not.toHaveBeenCalled();
    if (failure) reject(new Error("offline"));
    else resolve(new Response('{"status":"queued"}', { status: 202 }));
    await pending;
    expect(stopped).toHaveBeenCalledOnce();
    visibility(false);
    now += 120_000;
    window.dispatchEvent(new Event("pagehide"));
    await vi.advanceTimersByTimeAsync(1_800_000);
    await stop();
    expectBucket("1_to_5m");
  });
});
