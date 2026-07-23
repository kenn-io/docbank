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

describe("background jobs drawer", () => {
  it("distinguishes running work from a terminal failure", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          items: [
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
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    const close = vi.fn();

    render(JobsDrawer, {
      session: "short-lived",
      onclose: close,
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("extract:plain-text")).toBeTruthy();
    expect(screen.getByText("watch:inbox")).toBeTruthy();
    expect(screen.getByText("Still running")).toBeTruthy();
    expect(screen.getByText("source is unavailable")).toBeTruthy();

    await fireEvent.click(
      screen.getByRole("button", { name: "Close background jobs" }),
    );
    expect(close).toHaveBeenCalledOnce();
  });
});
