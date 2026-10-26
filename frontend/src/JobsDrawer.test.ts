import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import JobsDrawer from "./JobsDrawer.svelte";

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
      lane: item.lane,
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
  it("lists every worker below the running work and pauses idle ones", async () => {
    let paused = false;
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, options) => {
      if (options?.method === "PUT") paused = JSON.parse(String(options.body)).paused;
      return jobsResponse(
        [
          {
            name: "derive:visual-previews",
            lane: "derive:visual-previews",
            status: "running",
            busy: false,
            started_at: "2026-07-23T12:00:00Z",
          },
          {
            name: "extract:plain-text",
            lane: "extract:plain-text",
            status: "running",
            busy: false,
            started_at: "2026-07-23T12:00:00Z",
          },
        ],
        [
          {
            lane: "derive:visual-previews",
            paused,
            concurrency: 1,
            revision: paused ? 2 : 1,
            can_set_concurrency: true,
          },
        ],
      );
    });
    render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
    expect(await screen.findByText("Nothing running")).toBeTruthy();
    expect(screen.queryByRole("region")).toBeNull();
    expect(document.querySelector("details")).toBeNull();
    const workers = screen.getByRole("table", { name: "All workers" });
    expect(
      screen.getByText("Nothing running").compareDocumentPosition(workers) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(screen.getByRole("row", { name: "Extracting text Idle" })).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Pause Making previews" }));
    await waitFor(() =>
      expect(screen.getByRole("row", { name: /Making previews Paused/ })).toBeTruthy(),
    );
    expect(screen.getByRole("button", { name: "Resume Making previews" })).toBeTruthy();
    const write = fetchSpy.mock.calls.find(([, options]) => options?.method === "PUT")!;
    expect(String(write[0])).toContain("/lanes/derive%3Avisual-previews");
  });

  it.each(["paused", "failed", "waiting"])(
    "keeps an idle %s worker below Nothing running",
    async (state) => {
      vi.spyOn(globalThis, "fetch").mockResolvedValue(
        jobsResponse(
          [
            {
              name: "extract:plain-text",
              lane: "extract:plain-text",
              status: state === "failed" ? "failed" : "running",
              busy: false,
              started_at: "2026-07-23T12:00:00Z",
              error: state === "paused" ? undefined : "source is unavailable",
            },
          ],
          [
            {
              lane: "extract:plain-text",
              paused: state === "paused",
              concurrency: 1,
              revision: 1,
              can_set_concurrency: false,
            },
          ],
        ),
      );
      render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
      const label = state === "paused" ? "Paused" : state === "failed" ? "Failed" : "Waiting";
      expect(
        await screen.findByRole("row", { name: new RegExp(`Extracting text ${label}`) }),
      ).toBeTruthy();
      expect(screen.getByText("Nothing running")).toBeTruthy();
      expect(screen.queryByRole("region")).toBeNull();
      if (state !== "paused") expect(screen.getByText("source is unavailable")).toBeTruthy();
    },
  );

  it.each(["initial load", "lane controls"])(
    "shows a retry for unavailable %s and recovers to the empty state",
    async (failure) => {
      vi.spyOn(globalThis, "fetch")
        .mockResolvedValueOnce(
          failure === "initial load"
            ? new Response(JSON.stringify({ detail: "busy" }), {
                status: 503,
                headers: { "Content-Type": "application/json" },
              })
            : new Response(
                JSON.stringify({ items: [], lane_controls_error: "Lane controls are unavailable" }),
                { headers: { "Content-Type": "application/json" } },
              ),
        )
        .mockResolvedValueOnce(jobsResponse([]));
      render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
      expect((await screen.findByRole("alert")).textContent).toBe(
        failure === "initial load" ? "busy" : "Lane controls are unavailable",
      );
      expect(screen.queryByText("Nothing running")).toBeNull();
      await fireEvent.click(screen.getByRole("button", { name: "Try again" }));
      expect(await screen.findByText("Nothing running")).toBeTruthy();
      expect(screen.queryByRole("alert")).toBeNull();
      expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();
    },
  );

  it.each([
    ["initial load", 503, { detail: "busy" }, "busy"],
    [
      "lane controls",
      200,
      {
        items: [
          {
            name: "storage:a",
            kind: "photo_import",
            lane: "photo_import",
            operation_id: "a",
            status: "running",
            busy: true,
            started_at: "2026-07-23T12:00:00Z",
            can_cancel: true,
          },
        ],
        lane_controls_error: "Lane controls are unavailable",
      },
      "Lane controls are unavailable",
    ],
  ])(
    "keeps the unavailable %s error while a poll is pending",
    async (_failure, status, body, message) => {
      vi.useFakeTimers();
      try {
        vi.spyOn(globalThis, "fetch")
          .mockResolvedValueOnce(
            new Response(JSON.stringify(body), {
              status,
              headers: { "Content-Type": "application/json" },
            }),
          )
          .mockReturnValueOnce(new Promise<Response>(() => {}));
        render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
        await vi.advanceTimersByTimeAsync(0);
        expect(screen.getByRole("alert").textContent).toBe(message);
        await vi.advanceTimersByTimeAsync(2000);
        expect(globalThis.fetch).toHaveBeenCalledTimes(2);
        expect(screen.getByRole("alert").textContent).toBe(message);
        expect(screen.queryByText("Nothing running")).toBeNull();
      } finally {
        vi.useRealTimers();
      }
    },
  );

  it("shows unavailable lane controls while keeping progress and cancellation", async () => {
    const item = {
      name: "storage:a",
      kind: "photo_import",
      lane: "photo_import",
      operation_id: "a",
      status: "running",
      busy: true,
      started_at: "2026-07-23T12:00:00Z",
      completed_objects: 1,
      total_objects: 2,
      can_cancel: true,
    };
    let available = false;
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(async () =>
      available
        ? jobsResponse(
            [item],
            [
              {
                lane: "photo_import",
                paused: true,
                concurrency: 1,
                revision: 2,
                can_set_concurrency: false,
              },
            ],
          )
        : new Response(
            JSON.stringify({
              items: [item],
              lane_controls_error: "Lane controls are unavailable",
            }),
            { headers: { "Content-Type": "application/json" } },
          ),
    );
    render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
    expect((await screen.findByRole("alert")).textContent).toBe("Lane controls are unavailable");
    expect(screen.getByText("1 of 2 groups")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Cancel Importing photos a" }));
    const cancel = fetchSpy.mock.calls.find(([url]) =>
      String(url).endsWith("/api/v1/jobs/a/cancel"),
    );
    expect(cancel?.[1]?.method).toBe("POST");
    available = true;
    await fireEvent.click(screen.getByRole("button", { name: "Refresh background jobs" }));
    expect(await screen.findByRole("button", { name: "Resume Importing photos" })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
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
        lane: "place",
        operation_id: "a",
        status: "completed",
        started_at: "2026-07-23T12:00:00Z",
      },
      {
        name: "extract:plain-text",
        lane: "extract:plain-text",
        status: "completed",
        started_at: "2026-07-23T12:00:00Z",
      },
    ];
    const auth = vi.fn();
    const close = vi.fn();
    vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, options) =>
      options?.method === "PUT"
        ? new Response("{}", { status: 401, headers: { "Content-Type": "application/json" } })
        : jobsResponse(items, [control]),
    );
    render(JobsDrawer, { session: "short-lived", onclose: close, onauthfailure: auth });
    await fireEvent.click(
      await screen.findByRole("button", { name: "Resume Moving stored files" }),
    );
    await waitFor(() => expect(auth).toHaveBeenCalledOnce());
    expect(close).toHaveBeenCalledOnce();
    expect(screen.getAllByText("Paused").length).toBeGreaterThan(0);
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
      expect(screen.getAllByText("Paused").length).toBeGreaterThan(0);
      expect(screen.getByRole("button", { name: "Pause Repairing storage" })).toBeTruthy();
      await fireEvent.click(
        screen.getAllByRole("button", { name: "Resume Moving stored files" })[0],
      );
      await vi.advanceTimersByTimeAsync(0);
      expect(screen.getByRole("alert").textContent).toContain("changed elsewhere");
      await vi.advanceTimersByTimeAsync(2000);
      expect(screen.getByRole("alert").textContent).toContain("changed elsewhere");
      const writes = fetchSpy.mock.calls.filter(([, options]) => options?.method === "PUT");
      expect(writes).toHaveLength(1);
      expect(new Headers(writes[0][1]?.headers).get("If-Match")).toBe('"4"');
      await fireEvent.click(screen.getByRole("button", { name: "Pause Repairing storage" }));
      await vi.advanceTimersByTimeAsync(0);
      expect(screen.getByRole("button", { name: "Resume Repairing storage" })).toBeTruthy();
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
      lane: "derive:visual-previews",
      status: "running",
      busy: true,
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
      await screen.findByRole("combobox", { name: "Workers for Making previews: 1 worker" }),
    );
    await fireEvent.click(screen.getByRole("option", { name: "3 workers" }));
    expect(
      screen.getByRole("button", { name: "Pause Making previews" }).hasAttribute("disabled"),
    ).toBe(true);
    const write = fetchSpy.mock.calls.find(([, options]) => options?.method === "PUT")!;
    expect(JSON.parse(String(write[1]?.body))).toEqual({ paused: false, concurrency: 3 });
    complete(jobsResponse([]));
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Pause Making previews" }).hasAttribute("disabled"),
      ).toBe(false),
    );
  });

  it("distinguishes running work from a terminal failure", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      jobsResponse([
        {
          name: "extract:plain-text",
          lane: "extract:plain-text",
          status: "running",
          busy: true,
          started_at: "2026-07-23T12:00:00Z",
        },
        {
          name: "watch:inbox",
          lane: "watch:inbox",
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

    expect(await screen.findByRole("region", { name: "Extracting text" })).toBeTruthy();
    const failed = within(screen.getByRole("row", { name: /Watching inbox Failed/ }));
    expect(failed.getByText("Failed")).toBeTruthy();
    expect(failed.getByText("source is unavailable")).toBeTruthy();
    expect(screen.getByText("1 running")).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Close background jobs" }));
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
          lane: "photo_import",
          operation_id: id,
          status: "running",
          busy: true,
          started_at: "2026-07-23T12:00:00Z",
          completed_objects: 75,
          total_objects: 300,
          can_cancel: true,
        },
        {
          name: "storage:b",
          kind: "photo_import",
          lane: "photo_import",
          operation_id: "b",
          status: "queued",
          started_at: "2026-07-23T12:00:20Z",
          total_objects: 1,
          can_cancel: true,
        },
        {
          name: "storage:checkout",
          kind: "photo_checkout",
          lane: "photo_checkout",
          operation_id: "checkout",
          status: "running",
          busy: true,
          started_at: "2026-07-23T12:00:30Z",
          completed_objects: 1,
          total_objects: 2,
        },
        {
          name: `storage:${placeId}`,
          kind: "place",
          lane: "place",
          operation_id: placeId,
          status: "queued",
          started_at: "2026-07-23T12:01:00Z",
          can_cancel: true,
        },
        {
          name: "watch:inbox",
          lane: "watch:inbox",
          status: "running",
          busy: true,
          started_at: "2026-07-23T11:00:00Z",
        },
      ]),
    );

    render(JobsDrawer, {
      session: "short-lived",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    const progress = await screen.findByRole("progressbar", { name: "Importing photos progress" });
    expect(progress.getAttribute("aria-valuenow")).toBe("75");
    expect(progress.getAttribute("aria-valuemax")).toBe("301");
    expect(screen.getByRole("region", { name: "Importing photos" })).toBeTruthy();
    expect(screen.getByText("75 of 301 groups")).toBeTruthy();
    expect(screen.getByText("1 of 2 files")).toBeTruthy();
    expect(screen.getAllByRole("button", { name: /^Cancel / })).toHaveLength(3);
    expect(screen.getByText(id).tagName).toBe("CODE");
    expect(screen.getByRole("button", { name: `Cancel Importing photos ${id}` })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cancel Importing photos b" })).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: /^Cancel Moving stored files/ }));
    const cancel = fetchSpy.mock.calls.find(([url]) =>
      String(url).endsWith(`/api/v1/jobs/${placeId}/cancel`),
    );
    expect(cancel).toBeTruthy();
    expect(cancel?.[1]?.method).toBe("POST");
    expect(new Headers(cancel?.[1]?.headers).has("If-Match")).toBe(false);
  });

  it("refreshes while work is active and keeps polling after a transient error", async () => {
    vi.useFakeTimers();
    try {
      const active = {
        name: "extract:plain-text",
        lane: "extract:plain-text",
        status: "running",
        busy: true,
        started_at: "2026-07-23T12:00:00Z",
      };
      const fetchSpy = vi
        .spyOn(globalThis, "fetch")
        .mockResolvedValueOnce(jobsResponse([active]))
        .mockResolvedValueOnce(
          new Response(JSON.stringify({ message: "busy" }), {
            status: 503,
            headers: { "Content-Type": "application/json" },
          }),
        )
        .mockResolvedValueOnce(
          jobsResponse([
            { ...active, busy: false, status: "completed", finished_at: "2026-07-23T12:05:00Z" },
          ]),
        );

      render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
      await vi.advanceTimersByTimeAsync(0);
      expect(fetchSpy).toHaveBeenCalledTimes(1);
      expect(screen.getByRole("region", { name: "Extracting text" })).toBeTruthy();
      await vi.advanceTimersByTimeAsync(2000);
      expect(fetchSpy).toHaveBeenCalledTimes(2);
      await vi.advanceTimersByTimeAsync(2000);
      expect(fetchSpy).toHaveBeenCalledTimes(3);
      expect(screen.getByText("Nothing running")).toBeTruthy();
      expect(screen.queryByRole("region", { name: "Extracting text" })).toBeNull();
      cleanup();
      await vi.advanceTimersByTimeAsync(4000);
      expect(fetchSpy).toHaveBeenCalledTimes(3);
    } finally {
      vi.useRealTimers();
    }
  });

  it("shows checkout verification bytes and final entry results", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jobsResponse(
        [
          {
            name: "storage:scan",
            operation_id: "scan",
            kind: "photo_checkout_scan",
            lane: "photo_checkout",
            status: "running",
            busy: true,
            started_at: "2026-01-01T00:00:00Z",
            total_objects: 2,
            completed_objects: 0,
            processed_bytes: 1024,
            total_bytes: 4096,
          },
          {
            name: "storage:commit",
            operation_id: "commit",
            kind: "photo_checkout_commit",
            lane: "photo_checkout",
            status: "failed",
            started_at: "2026-01-01T00:00:00Z",
            checkout_results: { clean: 2, conflict: 1 },
          },
        ],
        [],
      ),
    );
    render(JobsDrawer, { session: "short-lived", onclose: vi.fn(), onauthfailure: vi.fn() });
    expect(await screen.findByText("Verified 1.00 KiB of 4.00 KiB")).toBeTruthy();
    const checkout = within(screen.getByRole("region", { name: "Photo checkout" }));
    expect(checkout.getByText("Checking files")).toBeTruthy();
    expect(checkout.getByText("Saving changes")).toBeTruthy();
    expect(screen.getByText("2 clean, 1 conflict")).toBeTruthy();
  });
});

it("shows current running work ahead of a retained failure in the same lane", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    jobsResponse([
      {
        name: "storage:old",
        kind: "place",
        lane: "place",
        operation_id: "old",
        status: "failed",
        started_at: "2026-07-23T11:00:00Z",
        error: "Earlier operation failed",
      },
      {
        name: "storage:current",
        kind: "place",
        lane: "place",
        operation_id: "current",
        status: "running",
        busy: true,
        started_at: "2026-07-23T12:00:00Z",
      },
    ]),
  );
  render(JobsDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  const lane = await screen.findByRole("region", { name: "Moving stored files" });
  expect(screen.getByText("1 running")).toBeTruthy();
  expect(within(lane).getByText("Earlier operation failed")).toBeTruthy();
  expect(screen.getByRole("row", { name: "Moving stored files Running" })).toBeTruthy();
});

it("labels queued work separately and counts only busy lanes as running", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    jobsResponse([
      {
        name: "storage:queued",
        lane: "photo_import",
        status: "queued",
        busy: false,
        started_at: "2026-07-23T12:00:00Z",
      },
      {
        name: "extract:plain-text",
        lane: "extract:plain-text",
        status: "running",
        busy: true,
        started_at: "2026-07-23T12:00:00Z",
      },
    ]),
  );
  render(JobsDrawer, { session: "synthetic", onclose: vi.fn(), onauthfailure: vi.fn() });
  const queued = await screen.findByRole("region", { name: "Importing photos" });
  expect(within(queued).getByText("Queued")).toBeTruthy();
  expect(screen.getByText("1 running")).toBeTruthy();
});
