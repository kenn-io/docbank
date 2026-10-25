import * as actions from "./action-events.js";
import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { act, cleanup, render, screen } from "@testing-library/svelte";
import SimilarDrawer from "./SimilarDrawer.svelte";
import * as processingAPI from "./receipts.js";
import contract from "../../internal/daemonconn/testdata/processing_responses.json";
import type { DocumentSimilarReport } from "./generated/docbank.js";
import { plan, versionID } from "./processing-test-fixtures.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const report = contract.similar_report as DocumentSimilarReport;

function setup(scope: string[] = [versionID], bindings = ["semantic"]) {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const path = String(input);
    if (path === "/api/v1/processing/profiles")
      return Response.json([
        {
          name: "private",
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: bindings,
          query_embedding_bindings: [],
        },
      ]);
    if (path === "/api/v1/processing/plans")
      return Response.json(plan(JSON.parse(String(init?.body)).selector.profile));
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
  it("searches the loaded view and groups identical copies", async () => {
    const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
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
    expect(action).toHaveBeenCalledExactlyOnceWith("short-lived", "search_run", "semantic");
  });

  it("stays silent when the file has no vectors yet", async () => {
    const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
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

  it("asks to narrow a view above the comparison limit", async () => {
    const similar = vi.spyOn(processingAPI, "documentSimilar");
    setup(Array.from({ length: 4097 }, (_, index) => `version-${index}`));
    expect(await screen.findByText("Narrow this view to at most 4,096 files.")).toBeTruthy();
    expect(similar).not.toHaveBeenCalled();
  });

  it("points to setup when no profile stores vectors", async () => {
    setup([versionID], []);
    expect(await screen.findByRole("link", { name: "Set one up" })).toBeTruthy();
  });

  it("ignores a result that arrives after close", async () => {
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
    expect(view.props.onclose).not.toHaveBeenCalled();
    expect(screen.queryByText(report.results[0]!.path)).toBeNull();
  });
});
