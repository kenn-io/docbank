import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/svelte";
import JobsDrawer from "./JobsDrawer.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function jobsResponse(items: unknown[]): Response {
  return new Response(JSON.stringify({ items }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

describe("background jobs drawer", () => {
  it("distinguishes running work from a terminal failure", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      jobsResponse([
        {
          name: "extract:plain-text",
          status: "running",
          started_at: "2026-07-23T12:00:00Z",
        },
        {
          name: "watch:inbox",
          status: "failed",
          started_at: "2026-07-23T11:00:00Z",
          finished_at: "2026-07-23T11:02:00Z",
          error: "source is unavailable",
        },
      ]),
    );
    const close = vi.fn();

    render(JobsDrawer, {
      session: "short-lived",
      onclose: close,
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("extract:plain-text")).toBeTruthy();
    expect(screen.getByText("watch:inbox")).toBeTruthy();
    expect(screen.getByText("failed")).toBeTruthy();
    expect(screen.getByText("source is unavailable")).toBeTruthy();

    await fireEvent.click(
      screen.getByRole("button", { name: "Close background jobs" }),
    );
    expect(close).toHaveBeenCalledOnce();
  });

  it("shows durable job progress and cancels through the shared Jobs route", async () => {
    const id = "f6730699-23b5-458a-b6d9-11b140ac1f92";
    const placeId = "0b9d8f43-6f0e-4b4a-9d55-2f1c5b2b7c11";
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async () =>
      jobsResponse([
        {
          name: `storage:${id}`,
          kind: "photo_import",
          operation_id: id,
          status: "running",
          started_at: "2026-07-23T12:00:00Z",
          completed_objects: 75,
          total_objects: 300,
          can_cancel: true,
        },
        {
          name: `storage:${placeId}`,
          kind: "place",
          operation_id: placeId,
          status: "queued",
          started_at: "2026-07-23T12:01:00Z",
          can_cancel: true,
        },
        {
          name: "watch:inbox",
          status: "running",
          started_at: "2026-07-23T11:00:00Z",
        },
      ]),
    );

    render(JobsDrawer, {
      session: "short-lived",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    const progress = await screen.findByRole("progressbar", { name: "Job progress" });
    expect(progress.getAttribute("aria-valuenow")).toBe("75");
    expect(progress.getAttribute("aria-valuemax")).toBe("300");
    expect(screen.getByText("Photo import")).toBeTruthy();
    expect(screen.getByText("75 of 300 groups")).toBeTruthy();
    expect(screen.getAllByRole("button", { name: /^Cancel / })).toHaveLength(2);

    await fireEvent.click(screen.getByRole("button", { name: `Cancel storage:${placeId}` }));
    const cancel = fetchSpy.mock.calls.find(([url]) => String(url).endsWith(`/api/v1/jobs/${placeId}/cancel`));
    expect(cancel).toBeTruthy();
    expect(cancel?.[1]?.method).toBe("POST");
    expect(new Headers(cancel?.[1]?.headers).has("If-Match")).toBe(false);
  });

  it("refreshes while work is active and keeps polling after a transient error", async () => {
    vi.useFakeTimers();
    try {
      const active = { name: "extract:plain-text", status: "running", started_at: "2026-07-23T12:00:00Z" };
      const fetchSpy = vi.spyOn(globalThis, "fetch")
        .mockResolvedValueOnce(jobsResponse([active]))
        .mockResolvedValueOnce(new Response(JSON.stringify({ message: "busy" }), { status: 503, headers: { "Content-Type": "application/json" } }))
        .mockResolvedValueOnce(jobsResponse([{ ...active, status: "completed", finished_at: "2026-07-23T12:05:00Z" }]));

      render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
      await vi.advanceTimersByTimeAsync(0);
      expect(fetchSpy).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(2000);
      expect(fetchSpy).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(2000);
      expect(fetchSpy).toHaveBeenCalledTimes(3);
      await vi.advanceTimersByTimeAsync(4000);
      expect(fetchSpy).toHaveBeenCalledTimes(3);
    } finally {
      vi.useRealTimers();
    }
  });
});
