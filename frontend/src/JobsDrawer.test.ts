import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import type { Job } from "./generated/docbank.js";
import JobsDrawer from "./JobsDrawer.svelte";

const jobFixture = (fields: Partial<Job>): Job => ({
  name: "storage:a",
  status: "running",
  started_at: "2026-07-23T12:00:00Z",
  controllable: false,
  paused: false,
  can_set_concurrency: false,
  ...fields,
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

function jobsResponse(
  items: unknown[],
  lanes = items
    .filter((item: any) => item.controllable)
    .map((item: any) => ({
      lane: item.kind ?? item.name,
      paused: item.paused,
      concurrency: item.concurrency,
      revision: item.control_revision,
      can_set_concurrency: item.can_set_concurrency ?? false,
    })),
): Response {
  return new Response(JSON.stringify({ items, lanes }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

describe("background jobs drawer", () => {
  it.each([
    {
      name: "recovers an unavailable initial load to the empty state after retry",
      populated: false,
      error: "busy",
      retry: "Try again",
    },
    {
      name: "keeps progress while lane controls are unavailable, then recovers on refresh",
      populated: true,
      error: "Lane controls are unavailable",
      retry: "Refresh background jobs",
    },
  ])("$name", async (test) => {
    vi.useFakeTimers();
    try {
      const items = test.populated
        ? [
            jobFixture({
              name: "storage:a",
              kind: "photo_import",
              operation_id: "a",
              completed_objects: 1,
              total_objects: 2,
              can_cancel: true,
            }),
          ]
        : [];
      const control = {
        lane: "photo_import",
        paused: true,
        concurrency: 1,
        revision: 2,
        can_set_concurrency: false,
      };
      const fetchSpy = vi.spyOn(globalThis, "fetch");
      const responses = [
        Promise.resolve(
          new Response(
            JSON.stringify(
              test.populated ? { items, lane_controls_error: test.error } : { detail: test.error },
            ),
            {
              status: test.populated ? 200 : 503,
              headers: { "Content-Type": "application/json" },
            },
          ),
        ),
        new Promise<Response>(() => {}),
        Promise.resolve(jobsResponse(items, test.populated ? [control] : [])),
      ];
      fetchSpy.mockImplementation(() => responses.shift()!);
      render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
      await vi.advanceTimersByTimeAsync(0);
      expect(screen.getByRole("alert").textContent).toBe(test.error);
      expect(screen.queryByText("No background jobs")).toBeNull();
      await vi.advanceTimersByTimeAsync(2000);
      expect(fetchSpy).toHaveBeenCalledTimes(2);
      expect(screen.getByRole("alert").textContent).toBe(test.error);
      expect(screen.queryByText("No background jobs")).toBeNull();
      expect(screen.queryByText("Read-only")).toBeNull();
      if (test.populated) {
        expect(screen.getByText("1 running · 1 total")).toBeTruthy();
        expect(screen.getByText("1 of 2 groups")).toBeTruthy();
        expect(
          (screen.getByRole("button", { name: "Cancel Photo import a" }) as HTMLButtonElement)
            .disabled,
        ).toBe(false);
      }
      await fireEvent.click(screen.getByRole("button", { name: test.retry }));
      await vi.advanceTimersByTimeAsync(0);
      if (test.populated) {
        expect(screen.getByRole("button", { name: "Resume Photo import" })).toBeTruthy();
      } else {
        expect(screen.getByText("No background jobs")).toBeTruthy();
        expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
      }
      expect(screen.queryByRole("alert")).toBeNull();
      expect(fetchSpy).toHaveBeenCalledTimes(3);
    } finally {
      vi.useRealTimers();
    }
  });

  it("refreshes while work is active, recovers polling errors, and stops on unmount", async () => {
    vi.useFakeTimers();
    try {
      const active = {
        name: "extract:plain-text",
        status: "running",
        started_at: "2026-07-23T12:00:00Z",
      };
      const fetchSpy = vi
        .spyOn(globalThis, "fetch")
        .mockResolvedValueOnce(jobsResponse([active]))
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ detail: "busy" }), {
            status: 503,
            headers: { "Content-Type": "application/json" },
          }),
        )
        .mockResolvedValueOnce(
          jobsResponse([{ ...active, status: "completed", finished_at: "2026-07-23T12:05:00Z" }]),
        );
      render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
      await vi.advanceTimersByTimeAsync(0);
      expect(screen.getByText("1 running · 1 total")).toBeTruthy();
      expect(fetchSpy).toHaveBeenCalledTimes(1);
      await vi.advanceTimersByTimeAsync(2000);
      expect(screen.getByRole("alert").textContent).toBe("busy");
      expect(screen.queryByText("No background jobs")).toBeNull();
      await vi.advanceTimersByTimeAsync(2000);
      expect(screen.getByText("Completed")).toBeTruthy();
      expect(screen.getByText("0 running · 1 total")).toBeTruthy();
      expect(screen.queryByRole("alert")).toBeNull();
      expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
      expect(fetchSpy).toHaveBeenCalledTimes(3);
      cleanup();
      await vi.advanceTimersByTimeAsync(4000);
      expect(fetchSpy).toHaveBeenCalledTimes(3);
    } finally {
      vi.useRealTimers();
    }
  });

  it("closes the drawer when a lane write loses authorization", async () => {
    const control = {
      lane: "place",
      paused: true,
      concurrency: 1,
      revision: 4,
      can_set_concurrency: false,
    };
    const items = [
      {
        name: "storage:a",
        kind: "place",
        operation_id: "a",
        status: "completed",
        started_at: "2026-07-23T12:00:00Z",
      },
      { name: "extract:plain-text", status: "completed", started_at: "2026-07-23T12:00:00Z" },
    ];
    const auth = vi.fn();
    const close = vi.fn();
    vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, options) =>
      options?.method === "PUT"
        ? new Response("{}", { status: 401, headers: { "Content-Type": "application/json" } })
        : jobsResponse(items, [control]),
    );
    render(JobsDrawer, { session: "short-lived", onclose: close, onauthfailure: auth });
    await fireEvent.click(await screen.findByRole("button", { name: "Resume Storage placement" }));
    await waitFor(() => expect(auth).toHaveBeenCalledOnce());
    expect(close).toHaveBeenCalledOnce();
    expect(screen.getByText("Paused")).toBeTruthy();
  });

  it("keeps a stale-revision warning across polls until another lane write succeeds", async () => {
    vi.useFakeTimers();
    try {
      const control = {
        lane: "place",
        paused: true,
        concurrency: 1,
        revision: 4,
        can_set_concurrency: false,
      };
      let repairPaused = false;
      const repair = () => ({
        lane: "repair",
        paused: repairPaused,
        concurrency: 1,
        revision: repairPaused ? 2 : 1,
        can_set_concurrency: false,
      });
      const headers = { "Content-Type": "application/json" };
      const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (url, options) => {
        if (options?.method !== "PUT") return jobsResponse([], [control, repair()]);
        if (String(url).endsWith("/lanes/place"))
          return new Response(JSON.stringify({ detail: "stale" }), { status: 412, headers });
        repairPaused = JSON.parse(String(options.body)).paused;
        return new Response(JSON.stringify(repair()), { headers });
      });
      render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
      await vi.advanceTimersByTimeAsync(0);
      expect(screen.getByText("Paused").parentElement?.textContent).toBe("Paused Idle");
      expect(screen.getByRole("button", { name: "Pause Storage repair" })).toBeTruthy();
      expect(screen.getAllByText("Idle")).toHaveLength(2);
      expect(screen.queryByText("No background jobs")).toBeNull();
      expect(screen.queryByText("Started")).toBeNull();
      expect(screen.queryByRole("progressbar")).toBeNull();
      await fireEvent.click(screen.getByRole("button", { name: "Resume Storage placement" }));
      await vi.advanceTimersByTimeAsync(0);
      expect(screen.getByRole("alert").textContent).toContain("changed elsewhere");
      await vi.advanceTimersByTimeAsync(2000);
      expect(screen.getByRole("alert").textContent).toContain("changed elsewhere");
      const writes = fetchSpy.mock.calls.filter(([, options]) => options?.method === "PUT");
      expect(writes).toHaveLength(1);
      expect(new Headers(writes[0][1]?.headers).get("If-Match")).toBe('"4"');
      await fireEvent.click(screen.getByRole("button", { name: "Pause Storage repair" }));
      await vi.advanceTimersByTimeAsync(0);
      expect(screen.getByRole("button", { name: "Resume Storage repair" })).toBeTruthy();
      const pause = fetchSpy.mock.calls.filter(([, options]) => options?.method === "PUT")[1];
      expect(JSON.parse(String(pause[1]?.body))).toEqual({ paused: true, concurrency: 1 });
      expect(screen.queryByRole("alert")).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("sets preview concurrency and disables controls while the write is pending", async () => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    Object.defineProperty(Element.prototype, "scrollIntoView", {
      configurable: true,
      value: vi.fn(),
    });
    const worker = {
      name: "derive:visual-previews",
      status: "running",
      started_at: "2026-07-23T12:00:00Z",
      controllable: true,
      paused: false,
      concurrency: 1,
      control_revision: 1,
      can_set_concurrency: true,
    };
    let complete!: (response: Response) => void;
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, options) =>
      options?.method === "PUT"
        ? new Promise<Response>((resolve) => {
            complete = resolve;
          })
        : jobsResponse([worker]),
    );
    render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
    await fireEvent.click(
      await screen.findByRole("combobox", { name: "Concurrency Visual previews: 1 worker" }),
    );
    await fireEvent.click(screen.getByRole("option", { name: "3 workers" }));
    expect(
      screen.getByRole("button", { name: "Pause Visual previews" }).hasAttribute("disabled"),
    ).toBe(true);
    const write = fetchSpy.mock.calls.find(([, options]) => options?.method === "PUT")!;
    expect(JSON.parse(String(write[1]?.body))).toEqual({ paused: false, concurrency: 3 });
    worker.concurrency = 3;
    worker.control_revision = 2;
    complete(
      new Response(
        JSON.stringify({
          lane: "derive:visual-previews",
          paused: false,
          concurrency: 3,
          revision: 2,
          can_set_concurrency: true,
        }),
        { headers: { "Content-Type": "application/json" } },
      ),
    );
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Pause Visual previews" }).hasAttribute("disabled"),
      ).toBe(false),
    );
    expect(
      screen.getByRole("combobox", { name: "Concurrency Visual previews: 3 workers" }),
    ).toBeTruthy();
  });

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

    expect(await screen.findByText("extract · plain text")).toBeTruthy();
    expect(screen.getByText("watch · inbox")).toBeTruthy();
    expect(screen.getByText("Failed")).toBeTruthy();
    expect(screen.getByText("Progress unavailable")).toBeTruthy();
    expect(screen.getAllByText("Read-only")).toHaveLength(2);
    expect(screen.getByText("Finished")).toBeTruthy();
    expect(screen.getByText("source is unavailable")).toBeTruthy();
    expect(screen.getByText("1 running · 2 total")).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Close background jobs" }));
    expect(close).toHaveBeenCalledOnce();
  });

  it("shows durable job progress and cancels through the shared Jobs route", async () => {
    const id = "f6730699-23b5-458a-b6d9-11b140ac1f92";
    const placeId = "0b9d8f43-6f0e-4b4a-9d55-2f1c5b2b7c11";
    const items: Job[] = [
      {
        ...jobFixture({ name: `storage:${id}`, kind: "photo_import", operation_id: id }),
        completed_objects: 75,
        total_objects: 300,
        can_cancel: true,
      },
      jobFixture({
        name: "storage:b",
        kind: "photo_import",
        operation_id: "b",
        status: "queued",
        started_at: "2026-07-23T12:00:20Z",
        total_objects: 1,
        can_cancel: true,
      }),
      jobFixture({
        name: "storage:checkout",
        kind: "photo_checkout",
        operation_id: "checkout",
        started_at: "2026-07-23T12:00:30Z",
        completed_objects: 1,
        total_objects: 2,
      }),
      {
        ...jobFixture({
          name: `storage:${placeId}`,
          kind: "place",
          operation_id: placeId,
          status: "queued",
          started_at: "2026-07-23T12:01:00Z",
        }),
        can_cancel: true,
      },
      jobFixture({ name: "watch:inbox", started_at: "2026-07-23T11:00:00Z" }),
    ];
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(
      async () =>
        new Response(JSON.stringify({ items }), {
          headers: { "Content-Type": "application/json" },
        }),
    );

    render(JobsDrawer, {
      session: "short-lived",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("75 of 300 groups")).toBeTruthy();
    const importProgress = screen.getByRole("progressbar", { name: "Photo import progress" });
    expect(importProgress.getAttribute("aria-valuenow")).toBe("75");
    expect(importProgress.getAttribute("aria-valuemax")).toBe("301");
    expect(screen.getByText("Photo import")).toBeTruthy();
    expect(screen.getByText("1 of 2 files")).toBeTruthy();
    expect(screen.getAllByRole("button", { name: /^Cancel / })).toHaveLength(3);
    expect(screen.getByText("2 operations")).toBeTruthy();
    expect(screen.getByText(id).tagName).toBe("CODE");
    expect(screen.getByRole("button", { name: `Cancel Photo import ${id}` })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cancel Photo import b" })).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: /^Cancel Storage placement/ }));
    const cancel = fetchSpy.mock.calls.find(([url]) =>
      String(url).endsWith(`/api/v1/jobs/${placeId}/cancel`),
    );
    expect(cancel).toBeTruthy();
    expect(cancel?.[1]?.method).toBe("POST");
    expect(new Headers(cancel?.[1]?.headers).has("If-Match")).toBe(false);
  });
});
