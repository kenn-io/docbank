import * as actions from "./action-events.js";
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import SimilarDrawer from "./SimilarDrawer.svelte";
import * as processingAPI from "./receipts.js";
import contract from "../../internal/daemonconn/testdata/processing_responses.json";
import type { DocumentSimilarReport } from "./generated/docbank.js";
import { plan, versionID } from "./processing-test-fixtures.js";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  vi.restoreAllMocks();
});

const report = contract.similar_report as DocumentSimilarReport;

function setup(scope: string[] = [versionID], bindings = ["semantic"], failFirstProfile = false) {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const path = String(input);
    if (path === "/api/v1/processing/profiles")
      return Response.json(
        (failFirstProfile ? ["private", "working"] : ["private"]).map((name) => ({
          name,
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: bindings,
          query_embedding_bindings: [],
        })),
      );
    if (path === "/api/v1/processing/plans") {
      const profile = JSON.parse(String(init?.body)).selector.profile;
      if (failFirstProfile && profile === "private")
        return Response.json({ detail: "Cannot read password file" }, { status: 422 });
      return Response.json(plan(profile));
    }
    throw new Error(`unexpected request: ${path}`);
  });
  const props = {
    session: "short-lived",
    node: {
      id: 42,
      name: "report.txt",
      kind: "file" as const,
      current_version_id: versionID,
      size: 64,
      revision: 1,
      created_at: "",
      modified_at: "",
    },
    path: "/Reports/report.txt",
    scopeVersionIDs: scope,
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  };
  return { ...render(SimilarDrawer, props), props };
}

describe("similar documents drawer", () => {
  it("can switch profiles after a plan fails", async () => {
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
    const similar = vi.spyOn(processingAPI, "documentSimilar").mockResolvedValue(report);
    setup([versionID], ["semantic"], true);
    expect(await screen.findByRole("alert")).toHaveProperty(
      "textContent",
      "Cannot read password file",
    );
    await fireEvent.click(screen.getByRole("combobox", { name: /Profile/ }));
    await fireEvent.click(await screen.findByRole("option", { name: "working" }));
    expect(await screen.findByText(report.results[0]!.path)).toBeTruthy();
    expect(similar).toHaveBeenCalledOnce();
    expect(similar.mock.calls[0]?.[1].selector.profile).toBe("working");
    expect(screen.queryByRole("alert")).toBeNull();
  });
  it("searches the loaded view and groups identical copies", async () => {
    const action = vi.spyOn(actions, "reportSearch").mockImplementation(() => {});
    const similar = vi
      .spyOn(processingAPI, "documentSimilar")
      .mockResolvedValue({ ...report, results: [{ ...report.results[0]!, duplicate_count: 1 }] });
    setup([versionID, report.source.content_version_id]);
    expect(await screen.findByText("+1 identical")).toBeTruthy();
    expect(screen.getByText(report.results[0]!.path)).toBeTruthy();
    expect(similar.mock.calls[0]?.[1].fence.content_version_ids).toEqual([
      versionID,
      report.source.content_version_id,
    ]);
    expect(action).toHaveBeenCalledExactlyOnceWith("short-lived", "semantic", false);
  });

  it("stays silent when the file has no vectors yet", async () => {
    const action = vi.spyOn(actions, "reportSearch").mockImplementation(() => {});
    vi.spyOn(processingAPI, "documentSimilar").mockResolvedValue({
      ...report,
      state: "unavailable",
      results: [],
    });
    setup();
    expect(
      await screen.findByText("This file has no search vectors yet. Process it first."),
    ).toBeTruthy();
    expect(action).not.toHaveBeenCalled();
  });

  it("reports a failed similar search once", async () => {
    const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
    vi.spyOn(processingAPI, "documentSimilar").mockRejectedValue(new Error("Similar failed"));
    setup();
    expect(await screen.findByText("Similar failed")).toBeTruthy();
    expect(failure).toHaveBeenCalledExactlyOnceWith("short-lived", "search", expect.any(Error));
  });

  it("asks to narrow a view above the comparison limit", async () => {
    const similar = vi.spyOn(processingAPI, "documentSimilar");
    setup(Array.from({ length: 4097 }, (_, index) => `version-${index}`));
    expect(await screen.findByText("Narrow this view to at most 4,096 files.")).toBeTruthy();
    expect(similar).not.toHaveBeenCalled();
  });

  it("points to setup when no profile stores vectors", async () => {
    setup([versionID], []);
    expect((await screen.findByRole("link", { name: "Set one up" })).getAttribute("href")).toBe(
      "https://docbank.ai/docs/configuration/",
    );
  });

  it("ignores a result that arrives after close", async () => {
    const action = vi.spyOn(actions, "reportSearch").mockImplementation(() => {});
    let finish!: (value: DocumentSimilarReport) => void;
    const similar = vi.spyOn(processingAPI, "documentSimilar").mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
    const view = setup();
    await vi.waitFor(() => expect(similar).toHaveBeenCalledOnce());
    view.unmount();
    await act(async () => {
      finish(report);
      await similar.mock.results[0]!.value;
    });
    expect(action).not.toHaveBeenCalled();
  });
});
