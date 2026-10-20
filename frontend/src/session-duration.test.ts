import type { BrowserTelemetryReporter } from "@kenn-io/kit-ui/utils/browser-telemetry";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { startSessionReporting } from "./session-duration.js";

describe("session_ended reporting", () => {
  const reportVisit = vi.fn<BrowserTelemetryReporter["reportVisit"]>();
  let initialReason: "enabled" | "disabled" = "enabled";
  let quietChange: Parameters<BrowserTelemetryReporter["onQuietChange"]>[0];
  const reporter: Pick<BrowserTelemetryReporter, "reportVisit" | "onQuietChange"> = {
    reportVisit,
    onQuietChange: (listener) => {
      quietChange = listener;
      listener(initialReason !== "enabled", initialReason);
      return () => {
        quietChange = () => undefined;
      };
    },
  };
  let reporting: ReturnType<typeof startSessionReporting> | undefined;
  let now: number;
  let hidden: boolean;

  beforeEach(() => {
    localStorage.clear();
    initialReason = "enabled";
    reportVisit.mockClear();
    vi.useFakeTimers({ toFake: ["Date", "setTimeout", "clearTimeout"] });
    now = 0;
    hidden = false;
    vi.spyOn(performance, "now").mockImplementation(() => now);
    vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
    vi.setSystemTime(new Date("2026-03-04T10:00:00Z"));
  });

  afterEach(() => {
    reporting?.stop();
    reporting = undefined;
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  const sessionReports = () => reportVisit.mock.calls.map(([event]) => event);
  const expectBucket = (bucket: string) => {
    expect(reportVisit.mock.calls).toEqual([
      [
        {
          event: "session_ended",
          properties: { surface: "web", duration_bucket: bucket },
        },
        { keepalive: true },
      ],
    ]);
  };
  const visibility = (value: boolean) => {
    hidden = value;
    document.dispatchEvent(new Event("visibilitychange"));
  };

  it("reports nothing for a hidden-only page", async () => {
    hidden = true;
    reporting = startSessionReporting(reporter);
    now = 3_600_000;
    await vi.advanceTimersByTimeAsync(3_600_000);
    window.dispatchEvent(new Event("pagehide"));
    reporting.stop();
    expect(sessionReports()).toHaveLength(0);
  });

  it("sums twenty one-minute visible stretches into one session", async () => {
    reporting = startSessionReporting(reporter);
    for (let i = 0; i < 20; i++) {
      now += 60_000;
      visibility(true);
      now += 600_000;
      await vi.advanceTimersByTimeAsync(600_000);
      expect(sessionReports()).toHaveLength(0);
      visibility(false);
    }
    window.dispatchEvent(new Event("pagehide"));
    reporting.stop();
    expectBucket("5_to_30m");
  });

  it("ends the session after thirty hidden minutes without a second pagehide report", async () => {
    reporting = startSessionReporting(reporter);
    now = 120_000;
    visibility(true);
    now += 1_800_000;
    await vi.advanceTimersByTimeAsync(1_799_999);
    expect(sessionReports()).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1);
    expectBucket("1_to_5m");
    window.dispatchEvent(new Event("pagehide"));
    reporting.stop();
    expectBucket("1_to_5m");
  });

  it.each([
    [1, "under_1m"],
    [59_999, "under_1m"],
    [60_000, "1_to_5m"],
    [299_999, "1_to_5m"],
    [300_000, "5_to_30m"],
    [1_800_000, "5_to_30m"],
    [1_800_001, "over_30m"],
  ])("buckets %i visible milliseconds as %s", (elapsed, bucket) => {
    reporting = startSessionReporting(reporter);
    now = Number(elapsed);
    window.dispatchEvent(new Event("pagehide"));
    reporting.stop();
    expectBucket(String(bucket));
  });

  it.each([
    ["bfcache restore", 30_000, "under_1m"],
    ["visibilitychange with a suspended timer", 240_000, "1_to_5m"],
    ["pageshow with a suspended timer", 240_000, "1_to_5m"],
  ])("starts a fresh session on %s", (resume, elapsed, bucket) => {
    reporting = startSessionReporting(reporter);
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
    reporting.stop();
    expect(sessionReports().map((event) => event.properties.duration_bucket)).toEqual([
      "1_to_5m",
      bucket,
    ]);
  });

  it.each(["checking", "unavailable", "disabled", "blocked"])(
    "retains pending visits through %s teardown",
    (state) => {
      reporting = startSessionReporting(reporter);
      quietChange(true, "checking");
      if (state === "unavailable") quietChange(true, "unavailable");
      if (state === "blocked")
        vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
          throw new Error("blocked");
        });
      for (let index = 0; index < 65; index++) {
        now += 120_000;
        window.dispatchEvent(new Event("pagehide"));
        window.dispatchEvent(new Event("pageshow"));
      }
      reporting.stop();
      if (state === "blocked") {
        expect(reportVisit).toHaveBeenCalledTimes(65);
      } else {
        expect(reportVisit).not.toHaveBeenCalled();
        expect(Object.keys(localStorage)).toHaveLength(64);
        if (state === "disabled") initialReason = "disabled";
        reporting = startSessionReporting(reporter);
        expect(reportVisit).toHaveBeenCalledTimes(state === "disabled" ? 0 : 64);
        if (state !== "disabled")
          expect(sessionReports()[0]?.properties.duration_bucket).toBe("1_to_5m");
        expect(Object.keys(localStorage)).toHaveLength(0);
      }
    },
  );

  it("takes the final bucket once without sending it", () => {
    reporting = startSessionReporting(reporter);
    now = 120_000;
    visibility(true);
    expect(reporting.take()).toBe("1_to_5m");
    expect(reporting.take()).toBeUndefined();
    expect(sessionReports()).toHaveLength(0);
  });

  it("stops duration work when disabled, resumes when enabled, and removes listeners on stop", async () => {
    reporting = startSessionReporting(reporter);
    now = 120_000;
    quietChange(true, "disabled");
    expect(reporting.take()).toBeUndefined();
    visibility(true);
    expect(vi.getTimerCount()).toBe(0);
    window.dispatchEvent(new Event("pagehide"));
    expect(sessionReports()).toHaveLength(0);
    hidden = false;
    quietChange(false, "enabled");
    now += 120_000;
    window.dispatchEvent(new Event("pagehide"));
    expectBucket("1_to_5m");
    reporting.stop();
    visibility(false);
    window.dispatchEvent(new Event("pageshow"));
    now += 120_000;
    window.dispatchEvent(new Event("pagehide"));
    await vi.advanceTimersByTimeAsync(1_800_000);
    reporting.stop();
    expectBucket("1_to_5m");
  });
});
