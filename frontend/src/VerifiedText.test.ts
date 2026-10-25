import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import VerifiedText from "./VerifiedText.svelte";
import type { SelectedSource } from "./selectedSource.js";

const source: SelectedSource = {
  kind: "live",
  key: "selected-image",
  nodeID: 7,
  mutationRevision: 3,
  versionID: "11111111-1111-4111-8111-111111111111",
  blobHash: "a".repeat(64),
  size: 50,
  name: "scan.png",
  path: "/scan.png",
  mimeType: "image/png",
  modifiedAt: "2026-09-11T12:00:00Z",
};
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it.each(["unconfigured", "unprocessed", "failed", "verified_empty"] as const)(
  "offers the %s text state without an empty Find toolbar",
  async (state) => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json({
        state,
        source: {
          node_id: 7,
          revision: 3,
          version_id: source.versionID,
          blob_hash: source.blobHash,
          size: 50,
          media_type: "image/png",
        },
        profile: {
          name: "",
          configuration: state === "unconfigured" ? "unconfigured" : "configured",
          fingerprint: state === "unconfigured" ? "" : "b".repeat(64),
        },
        generation_id: "",
        attachment_id: "",
        build_id: "",
      }),
    );
    const onprocess = vi.fn();
    render(VerifiedText, {
      session: "session",
      source,
      authorizationRevision: 3,
      onauthfailure: vi.fn(),
      onprocess,
    });
    if (state === "unconfigured") {
      expect(await screen.findByText("Text extraction isn't set up.")).toBeTruthy();
      expect(screen.getByRole("link", { name: "Set up" }).getAttribute("href")).toBe(
        "https://docbank.ai/docs/usage/document-processing/",
      );
    } else if (state === "unprocessed") {
      await fireEvent.click(await screen.findByRole("button", { name: "Process…" }));
      expect(onprocess).toHaveBeenCalledOnce();
    } else if (state === "failed") {
      expect(
        await screen.findByText("Text processing failed for this selected version."),
      ).toBeTruthy();
    } else {
      expect(
        await screen.findByText("Processing verified that this document has no readable text."),
      ).toBeTruthy();
      expect(screen.getByLabelText("Verified")).toBeTruthy();
    }
    expect(screen.queryByRole("searchbox")).toBeNull();
  },
);
