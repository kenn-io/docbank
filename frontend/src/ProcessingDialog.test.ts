import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import ProcessingDialog from "./ProcessingDialog.svelte";
import * as processingAPI from "./receipts.js";
import type { ProcessingPlan } from "./generated/docbank.js";
import { doclingHop, plan, versionID } from "./processing-test-fixtures.js";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

const job = {
  id: "b".repeat(64),
  attachment_id: "c".repeat(64),
  embedding_job_ids: [],
  profile_fingerprint: "a".repeat(64),
  content_version_id: versionID,
};

function completed(): Response {
  return new Response(
    `${JSON.stringify({ sequence: 1, type: "job", job })}\n${JSON.stringify({
      sequence: 2,
      type: "status",
      status: {
        job_id: job.id,
        state: "completed",
        phase: "published",
        embedding_job_ids: [],
        completed_bindings: 0,
      },
      terminal: true,
    })}\n`,
    { headers: { "Content-Type": "application/x-ndjson" } },
  );
}

function setup(
  plans: Record<string, ProcessingPlan>,
  run: () => Promise<Response> = async () => completed(),
  initiallyGranted: string[] = [],
  failing: string[] = [],
) {
  const granted = new Set<string>(initiallyGranted);
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const path = String(input);
    if (path === "/api/v1/processing/profiles")
      return Response.json(
        Object.keys(plans).map((name) => ({
          name,
          fingerprint: "a".repeat(64),
          rendition: true,
          embedding_bindings: [],
          query_embedding_bindings: [],
        })),
      );
    if (path === "/api/v1/processing/plans") {
      const name = JSON.parse(String(init?.body)).selector.profile as string;
      if (failing.includes(name)) return new Response("plan failed", { status: 500 });
      const active = granted.has(name);
      return Response.json({
        ...plans[name],
        consent_required: active ? false : plans[name]!.consent_required,
        consent_state: active ? "active" : plans[name]!.consent_state,
      });
    }
    if (path.startsWith("/api/v1/coverage?"))
      return Response.json({
        vault_uid: plans[Object.keys(plans)[0]!]!.vault_uid,
        profile_fingerprint: "a".repeat(64),
        state: "partial",
        renditions: { name: "rendition", state: "missing", complete: 0, total: 1 },
        embeddings: [],
      });
    if (path === "/api/v1/processing/jobs") {
      const body = JSON.parse(String(init?.body));
      if (body.consent) granted.add(body.selector.profile);
      return run();
    }
    if (path === "/api/v1/processing/consent/revocations") {
      granted.clear();
      return Response.json({ revoked_at: "2026-01-01T00:00:00Z" });
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
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
    onrendition: vi.fn(),
  };
  return { ...render(ProcessingDialog, props), fetchMock, props };
}

async function enabledRun(): Promise<HTMLElement> {
  const run = await screen.findByRole("button", { name: "Extract text" });
  await waitFor(() => expect(run.hasAttribute("disabled")).toBe(false));
  return run;
}

function jobRequests(fetchMock: ReturnType<typeof setup>["fetchMock"]): { consent: boolean }[] {
  return fetchMock.mock.calls
    .filter(([input]) => input === "/api/v1/processing/jobs")
    .map(([, init]) => JSON.parse(String(init?.body)));
}

describe("processing dialog", () => {
  it("review: hides a PDF-only profile for an oversized text document", async () => {
    const { rerender, props } = setup({
      "pdf-only": plan("pdf-only", { rendition_ineligible_reason: "unsupported_media_type" }),
      text: plan("text", { rendition_ineligible_reason: "source_bytes_exceeded" }),
    });
    await rerender({ node: { ...props.node, mime_type: "text/plain", size: 2 ** 30 + 1 } });
    expect(await screen.findByText("This file is too large for this profile.")).toBeTruthy();
    expect(screen.queryByRole("combobox", { name: /Profile/ })).toBeNull();
    expect(screen.queryByText("pdf-only")).toBeNull();
  });

  it("offers transcription when the audio plan retains only text", async () => {
    const { rerender, props } = setup({ audio: plan("audio", { consent_required: false }) });
    await rerender({ node: { ...props.node, mime_type: "audio/wav" } });
    expect(await screen.findByRole("dialog", { name: "Transcribe" })).toBeTruthy();
    const run = screen.getByRole("button", { name: "Transcribe" });
    await waitFor(() => expect(run.hasAttribute("disabled")).toBe(false));
  });

  it.each(["unsupported_media_type", "photo_not_eligible"] as const)(
    "filters out %s profiles and starts on a local one",
    async (reason) => {
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
      setup({
        captions: plan("captions", { rendition_ineligible_reason: reason }),
        docling: plan("docling", { flow: [doclingHop] }),
        text: plan("text"),
      });
      expect(
        await screen.findByText("Runs on this computer. Nothing leaves Docbank."),
      ).toBeTruthy();
      const picker = screen.getByRole("combobox", { name: /Profile/ });
      await waitFor(() => expect(picker.hasAttribute("disabled")).toBe(false));
      expect(picker.textContent).toContain("text");
      await fireEvent.click(picker);
      const options = (await screen.findAllByRole("option")).map((option) =>
        option.textContent?.trim(),
      );
      expect(options).toEqual(["docling", "text"]);
    },
  );

  it("names a profile whose plan couldn't be built instead of dropping it silently", async () => {
    setup(
      { docling: plan("docling", { flow: [doclingHop] }), text: plan("text") },
      undefined,
      [],
      ["docling"],
    );
    expect(await screen.findByText(/Couldn't check docling/)).toBeTruthy();
    expect(screen.queryByText(/Couldn't check text/)).toBeNull();
  });

  it("says when no profile handles the file type", async () => {
    setup({
      captions: plan("captions", { rendition_ineligible_reason: "unsupported_media_type" }),
    });
    const line = await screen.findByText(/No processing profile handles this file type/);
    expect(within(line).getByRole("link", { name: "Set one up" }).getAttribute("href")).toBe(
      "https://docbank.ai/docs/configuration/",
    );
    expect(screen.queryByRole("button", { name: "Extract text" })).toBeNull();
  });

  it("requires consent before a profile's first run and not after", async () => {
    const view = setup({ text: plan("text") });
    const run = await screen.findByRole("button", { name: "Extract text" });
    expect(run.hasAttribute("disabled")).toBe(true);
    await fireEvent.click(
      screen.getByRole("checkbox", { name: "Allow “text” to process documents in this vault" }),
    );
    await waitFor(() => expect(run.hasAttribute("disabled")).toBe(false));
    await fireEvent.click(run);
    expect(await screen.findByText("Text extracted.")).toBeTruthy();
    expect(jobRequests(view.fetchMock).map((request) => request.consent)).toEqual([true]);
    await fireEvent.click(screen.getByRole("button", { name: "Read text" }));
    expect(view.props.onrendition).toHaveBeenCalledWith(job.attachment_id);
    expect(screen.queryByRole("checkbox")).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "Extract text" }));
    await waitFor(() => expect(jobRequests(view.fetchMock)).toHaveLength(2));
    expect(jobRequests(view.fetchMock)[1].consent).toBe(false);
  });

  it("blocks a run the file can't take and says why", async () => {
    setup({ docling: plan("docling", { rendition_ineligible_reason: "pdf_password_required" }) });
    expect(
      await screen.findByText("This PDF is locked. Add its password to the PDF password file."),
    ).toBeTruthy();
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(screen.getByRole("button", { name: "Extract text" }).hasAttribute("disabled")).toBe(
      true,
    );
  });

  it("keeps the job after a truncated status stream", async () => {
    let stream!: ReadableStreamDefaultController<Uint8Array>;
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        stream = controller;
      },
    });
    setup(
      { text: plan("text", { consent_required: false, consent_state: "active" }) },
      async () => new Response(body, { headers: { "Content-Type": "application/x-ndjson" } }),
    );
    await fireEvent.click(await enabledRun());
    stream.enqueue(
      new TextEncoder().encode(`${JSON.stringify({ sequence: 1, type: "job", job })}\n`),
    );
    stream.close();
    expect(await screen.findByRole("alert")).toHaveProperty(
      "textContent",
      "The processing stream did not end after its terminal status.",
    );
    expect(screen.getByText("Running in the background.")).toBeTruthy();
    await fireEvent.click(screen.getByText("Details"));
    expect(screen.getByRole("button", { name: "Copy Job ID" }).textContent).toContain(job.id);
  });

  it("ignores a run that finishes after the dialog closes", async () => {
    const start = vi.spyOn(processingAPI, "startProcessing");
    let finish!: (response: Response) => void;
    const view = setup(
      { text: plan("text", { consent_required: false, consent_state: "active" }) },
      () =>
        new Promise<Response>((resolve) => {
          finish = resolve;
        }),
    );
    await fireEvent.click(await enabledRun());
    await waitFor(() => expect(finish).toBeTypeOf("function"));
    view.unmount();
    await act(async () => {
      finish(Response.json({ detail: "Session expired" }, { status: 401 }));
      await start.mock.results[0]!.value.catch(() => {});
    });
    expect(view.props.onauthfailure).not.toHaveBeenCalled();
    expect(view.props.onclose).not.toHaveBeenCalled();
  });

  it("revokes every grant from Details and asks for consent again", async () => {
    setup({ text: plan("text") }, undefined, ["text"]);
    await fireEvent.click(await screen.findByText("Details"));
    expect(screen.getByText("Allowed")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy Plan ID" }).textContent).toContain(
      "f".repeat(64),
    );
    await fireEvent.click(
      screen.getByRole("button", { name: "Revoke all processing permissions" }),
    );
    expect(await screen.findByRole("checkbox")).toBeTruthy();
  });

  it("hands an expired session to the app", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json({ detail: "Session expired" }, { status: 401 }),
    );
    const onclose = vi.fn(),
      onauthfailure = vi.fn();
    render(ProcessingDialog, {
      session: "short-lived",
      node: {
        id: 42,
        name: "report.txt",
        kind: "file",
        current_version_id: versionID,
        size: 64,
        revision: 1,
        created_at: "",
        modified_at: "",
      },
      path: "/Reports/report.txt",
      onclose,
      onauthfailure,
      onrendition: vi.fn(),
    });
    await waitFor(() => expect(onauthfailure).toHaveBeenCalledOnce());
    expect(onclose).toHaveBeenCalledOnce();
  });
});
