import { APIError } from "./api-transport.js";
import { render, fireEvent, screen, cleanup } from "@testing-library/svelte";
import { afterEach, expect, it, vi } from "vite-plus/test";
import * as numbering from "./redactionNumbering.js";
import RedactionNumbering from "./RedactionNumbering.svelte";
import type { NumberingInputs } from "./redactionNumbering.js";
const ns = {
  namespace_id: "11111111-1111-4111-8111-111111111111",
  prefix: "CASE",
  suffix: "",
  padding: 6,
  created_at: "2026-01-01T00:00:00Z",
};
const inputs = { review: { resolved: { page_count: 2 } } } as NumberingInputs;
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
it("reads the used cursor before enabling confirmation and requires separate retention consent", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(
    async (url) =>
      new Response(
        JSON.stringify(
          String(url).endsWith(ns.namespace_id)
            ? { namespace: ns, next_sequence: 3 }
            : { items: [ns], total: 1 },
        ),
      ),
  );
  render(RedactionNumbering, {
    session: "tab",
    inputs,
    title: "Synthetic.pdf",
    onauthfailure: vi.fn(),
    onpublished: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Number and keep PDF" }));
  await screen.findByDisplayValue("3");
  expect(screen.getByText("CASE000003–CASE000004")).toBeTruthy();
  const confirm = screen.getByRole("button", { name: "Review numbering" });
  expect((confirm as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.click(
    screen.getByRole("checkbox", { name: "Keep the unredacted original indefinitely" }),
  );
  expect((confirm as HTMLButtonElement).disabled).toBe(false);
  await fireEvent.click(confirm);
  const dialog = screen.getByRole("dialog", { name: "Keep this numbered PDF?" });
  await fireEvent.keyDown(dialog, { key: "Escape" });
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(screen.getByDisplayValue("3")).toBeTruthy();
});
it("does not guess a start when the namespace lookup fails", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url) =>
    String(url).endsWith(ns.namespace_id)
      ? new Response(JSON.stringify({ detail: "Try later" }), { status: 503 })
      : new Response(JSON.stringify({ items: [ns], total: 1 })),
  );
  render(RedactionNumbering, {
    session: "tab",
    inputs,
    title: "Synthetic.pdf",
    onauthfailure: vi.fn(),
    onpublished: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Number and keep PDF" }));
  await screen.findByText("Try later");
  expect(
    (screen.getByRole("button", { name: "Review numbering" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  expect(
    (screen.getByRole("button", { name: "Refresh next number" }) as HTMLButtonElement).disabled,
  ).toBe(false);
});

it("replays the frozen range after a lost reservation response, then retries only publication", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(
    async (url) =>
      new Response(
        JSON.stringify(
          String(url).endsWith(ns.namespace_id)
            ? { namespace: ns, next_sequence: 3 }
            : { items: [ns], total: 1 },
        ),
      ),
  );
  const reserve = vi
    .spyOn(numbering, "reserveNumberedRedaction")
    .mockRejectedValueOnce(new TypeError("Lost reply"))
    .mockResolvedValue({ allocation_id: "22222222-2222-4222-8222-222222222222" } as never);
  const publish = vi
    .spyOn(numbering, "publishNumberedRedaction")
    .mockRejectedValueOnce(new TypeError("Unavailable"))
    .mockResolvedValue({ pages: [{ label: "CASE000003" }, { label: "CASE000004" }] } as never);
  const completed = vi.fn();
  render(RedactionNumbering, {
    session: "tab",
    inputs,
    title: "Synthetic.pdf",
    onauthfailure: vi.fn(),
    onpublished: completed,
  });
  await fireEvent.click(screen.getByRole("button", { name: "Number and keep PDF" }));
  await screen.findByDisplayValue("3");
  await fireEvent.click(
    screen.getByRole("checkbox", { name: "Keep the unredacted original indefinitely" }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Review numbering" }));
  await fireEvent.click(screen.getByRole("button", { name: "Reserve and keep PDF" }));
  await screen.findByText("Lost reply");
  await fireEvent.click(screen.getByRole("button", { name: "Retry reservation" }));
  await screen.findByText("Unavailable");
  expect(reserve).toHaveBeenCalledTimes(2);
  expect(reserve.mock.calls[0]![1]).toEqual(reserve.mock.calls[1]![1]);
  expect(reserve.mock.calls[0]![1].start).toBe(3);
  await fireEvent.click(screen.getByRole("button", { name: "Retry publication" }));
  await screen.findByRole("button", { name: "Download numbered PDF" });
  expect(reserve).toHaveBeenCalledTimes(2);
  expect(publish.mock.calls[0]![2]).toEqual(publish.mock.calls[1]![2]);
  expect(completed).toHaveBeenCalledOnce();
});

it("keeps an explicit override until a conflict is deliberately refreshed and confirmed", async () => {
  let next = 3;
  vi.spyOn(globalThis, "fetch").mockImplementation(
    async (url) =>
      new Response(
        JSON.stringify(
          String(url).endsWith(ns.namespace_id)
            ? { namespace: ns, next_sequence: next }
            : { items: [ns], total: 1 },
        ),
      ),
  );
  const reserve = vi
    .spyOn(numbering, "reserveNumberedRedaction")
    .mockRejectedValue(new APIError("Range already reserved", 409, "bates_reservation_conflict"));
  render(RedactionNumbering, {
    session: "tab",
    inputs,
    title: "Synthetic.pdf",
    onauthfailure: vi.fn(),
    onpublished: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Number and keep PDF" }));
  const first = await screen.findByDisplayValue("3");
  await fireEvent.input(first, { target: { value: "8" } });
  expect(screen.getByText("CASE000008–CASE000009")).toBeTruthy();
  await fireEvent.click(
    screen.getByRole("checkbox", { name: "Keep the unredacted original indefinitely" }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Review numbering" }));
  await fireEvent.click(screen.getByRole("button", { name: "Reserve and keep PDF" }));
  await screen.findByText("Range already reserved");
  expect(screen.getByDisplayValue("8")).toBeTruthy();
  expect(reserve).toHaveBeenCalledOnce();
  next = 5;
  await fireEvent.click(screen.getByRole("button", { name: "Refresh next number" }));
  await screen.findByDisplayValue("5");
  expect(
    (screen.getByRole("button", { name: "Review numbering" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  next = 1000000;
  await fireEvent.click(screen.getByRole("button", { name: "Refresh next number" }));
  await screen.findByText("This namespace cannot fit 2 pages. Choose another namespace.");
  await fireEvent.click(
    screen.getByRole("checkbox", { name: "Keep the unredacted original indefinitely" }),
  );
  await fireEvent.input(screen.getByRole("textbox", { name: "First number" }), {
    target: { value: "1" },
  });
  expect(
    (screen.getByRole("button", { name: "Review numbering" }) as HTMLButtonElement).disabled,
  ).toBe(true);
});

it("cancels a publication request without dropping its allocation or frozen inputs", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(
    async (url) =>
      new Response(
        JSON.stringify(
          String(url).endsWith(ns.namespace_id)
            ? { namespace: ns, next_sequence: 3 }
            : { items: [ns], total: 1 },
        ),
      ),
  );
  const reservation = { allocation_id: "22222222-2222-4222-8222-222222222222" };
  const reserve = vi
    .spyOn(numbering, "reserveNumberedRedaction")
    .mockResolvedValue(reservation as never);
  let pendingSignal: AbortSignal | undefined;
  const publish = vi
    .spyOn(numbering, "publishNumberedRedaction")
    .mockImplementationOnce(
      (_session, _inputs, _reservation, signal) =>
        new Promise((_resolve, reject) => {
          pendingSignal = signal;
          signal!.addEventListener("abort", () => reject(signal!.reason), { once: true });
        }),
    )
    .mockResolvedValue({ pages: [{ label: "CASE000003" }, { label: "CASE000004" }] } as never);
  render(RedactionNumbering, {
    session: "tab",
    inputs,
    title: "Synthetic.pdf",
    onauthfailure: vi.fn(),
    onpublished: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Number and keep PDF" }));
  await screen.findByDisplayValue("3");
  await fireEvent.click(
    screen.getByRole("checkbox", { name: "Keep the unredacted original indefinitely" }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Review numbering" }));
  await fireEvent.click(screen.getByRole("button", { name: "Reserve and keep PDF" }));
  await screen.findByText("Rendering and keeping PDF…");
  await fireEvent.click(screen.getByRole("button", { name: "Cancel request" }));
  expect(pendingSignal?.aborted).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Retry publication" }));
  await screen.findByRole("button", { name: "Download numbered PDF" });
  expect(reserve).toHaveBeenCalledOnce();
  expect(publish.mock.calls[1]![1]).toEqual(publish.mock.calls[0]![1]);
  expect(publish.mock.calls[1]![2]).toEqual(reservation);
});
