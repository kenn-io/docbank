import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import AuditEvidenceDrawer from "./AuditEvidenceDrawer.svelte";

const evidence = {
  vault_id: "11111111-1111-4111-8111-111111111111",
  lineage_id: "22222222-2222-4222-8222-222222222222",
  operation_sequence_high_water: 18,
  allocation_entry_count: 18,
  allocation_head: "a".repeat(64),
  scopes: [
    {
      id: "33333333-3333-4333-8333-333333333333",
      entry_count: 7,
      chain_head: "b".repeat(64),
    },
  ],
};

function statusResponse(enabled = true): Response {
  return new Response(JSON.stringify({ enabled }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

let statusMock: ReturnType<typeof vi.fn<typeof fetch>>;
let verifyMock: ReturnType<typeof vi.fn<typeof fetch>>;

beforeEach(() => {
  statusMock = vi.fn<typeof fetch>(() => Promise.resolve(statusResponse()));
  verifyMock = vi.fn<typeof fetch>();
  vi.spyOn(globalThis, "fetch").mockImplementation((url, options) => {
    if (url === "/api/v1/audit/status") return statusMock(url, options);
    if (url === "/api/v1/audit/verify") return verifyMock(url, options);
    throw new Error(`Unexpected request: ${url}`);
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("audit evidence drawer", () => {
  it("shows independently verified authority and protected content", async () => {
    verifyMock.mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          enabled: true,
          evidence,
          protected_blobs: 3,
          protected_bytes: 4096,
          verified_blobs: 3,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );

    render(AuditEvidenceDrawer, {
      session: "short-lived",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("Protected history and content agree")).toBeTruthy();
    const summary = within(screen.getByLabelText("Verification summary"));
    expect(summary.getByText("4.00 KiB in 3 files")).toBeTruthy();
    expect(summary.getByText("3 of 3 files (SHA-256)")).toBeTruthy();
    expect(screen.getByText("Details").closest("details")!.open).toBe(false);
    expect(screen.getByText(evidence.lineage_id)).toBeTruthy();
    expect(screen.getByText(evidence.scopes[0]!.id)).toBeTruthy();
    expect(screen.getByText(evidence.scopes[0]!.chain_head)).toBeTruthy();
    expect(verifyMock).toHaveBeenCalledWith(
      "/api/v1/audit/verify",
      expect.objectContaining({
        method: "POST",
        body: "{}",
      }),
    );
  });

  it.each(["status", "verification"])(
    "distinguishes a dormant vault from %s without offering refresh",
    async (source) => {
      if (source === "status") statusMock.mockResolvedValueOnce(statusResponse(false));
      else {
        statusMock.mockRejectedValueOnce(new Error("status unavailable"));
        verifyMock.mockResolvedValueOnce(
          new Response(
            JSON.stringify({
              enabled: false,
              protected_blobs: 0,
              protected_bytes: 0,
              verified_blobs: 0,
            }),
            { status: 200 },
          ),
        );
      }

      render(AuditEvidenceDrawer, {
        session: "short-lived",
        onclose: vi.fn(),
        onauthfailure: vi.fn(),
      });

      expect(await screen.findByText(/No folders are protected/)).toBeTruthy();
      expect(screen.getByRole("button", { name: "Copy audit enable command" }).title).toBe(
        "docbank audit enable /folder",
      );
      expect(
        screen.queryByRole("button", { name: "Run permanent audit verification again" }),
      ).toBeNull();
      expect(statusMock).toHaveBeenCalledTimes(1);
      expect(verifyMock).toHaveBeenCalledTimes(source === "status" ? 0 : 1);
    },
  );

  it("verifies again on request and reports each problem", async () => {
    verifyMock
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            enabled: true,
            evidence,
            protected_blobs: 1,
            protected_bytes: 12,
            verified_blobs: 1,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            enabled: true,
            evidence,
            protected_blobs: 1,
            protected_bytes: 12,
            verified_blobs: 0,
            problems: [{ hash: "c".repeat(64), problem: "corrupt" }],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );

    render(AuditEvidenceDrawer, {
      session: "short-lived",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("Protected history and content agree")).toBeTruthy();
    await fireEvent.click(
      screen.getByRole("button", {
        name: "Run permanent audit verification again",
      }),
    );

    expect(await screen.findByText("Protected history needs attention")).toBeTruthy();
    expect(screen.getByText("corrupt")).toBeTruthy();
    expect(screen.getByText("c".repeat(64))).toBeTruthy();
    expect(verifyMock).toHaveBeenCalledTimes(2);
    expect(statusMock).toHaveBeenCalledTimes(2);
  });

  it("withdraws an earlier verified result while refresh is unresolved", async () => {
    let rejectRefresh!: (cause: Error) => void;
    const refreshResponse = new Promise<Response>((_resolve, reject) => {
      rejectRefresh = reject;
    });
    verifyMock
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            enabled: true,
            evidence,
            protected_blobs: 3,
            protected_bytes: 4096,
            verified_blobs: 3,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockReturnValueOnce(refreshResponse);

    render(AuditEvidenceDrawer, {
      session: "short-lived",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("Protected history and content agree")).toBeTruthy();
    await fireEvent.click(
      screen.getByRole("button", {
        name: "Run permanent audit verification again",
      }),
    );

    expect(screen.getByText("Verifying…")).toBeTruthy();
    expect(screen.queryByText("Protected history and content agree")).toBeNull();

    rejectRefresh(new Error("verification interrupted"));
    expect((await screen.findByRole("alert")).textContent).toContain("verification interrupted");
    expect(screen.queryByText("Protected history and content agree")).toBeNull();
  });

  it.each(["HTTP", "transport", "success"])(
    "reports damaged authority after %s status",
    async (failure) => {
      if (failure === "HTTP") {
        statusMock.mockResolvedValueOnce(
          new Response(JSON.stringify({ detail: "audit authority is incomplete" }), {
            status: 500,
          }),
        );
      } else if (failure === "transport") {
        statusMock.mockRejectedValueOnce(new Error("status unavailable"));
      }
      verifyMock.mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            enabled: false,
            protected_blobs: 0,
            protected_bytes: 0,
            verified_blobs: 0,
            metadata_problems: ["audit scope chain does not match its head"],
          }),
          { status: 200 },
        ),
      );
      render(AuditEvidenceDrawer, {
        session: "short-lived",
        onclose: vi.fn(),
        onauthfailure: vi.fn(),
      });
      expect(await screen.findByText("Protected history needs attention")).toBeTruthy();
      expect(screen.getByText("audit scope chain does not match its head")).toBeTruthy();
      expect(screen.queryByText(/No folders are protected/)).toBeNull();
      expect(verifyMock).toHaveBeenCalledTimes(1);
    },
  );

  it("closes and requests sign-in when status returns unauthorized", async () => {
    statusMock.mockResolvedValueOnce(new Response("Unauthorized", { status: 401 }));
    const onclose = vi.fn(),
      onauthfailure = vi.fn();
    render(AuditEvidenceDrawer, { session: "short-lived", onclose, onauthfailure });
    await waitFor(() => expect(onauthfailure).toHaveBeenCalledTimes(1));
    expect(onclose).toHaveBeenCalledTimes(1);
    expect(verifyMock).not.toHaveBeenCalled();
  });

  it("cancels the pending status request on close", async () => {
    statusMock.mockReturnValueOnce(new Promise<Response>(() => {}));
    const { unmount } = render(AuditEvidenceDrawer, {
      session: "short-lived",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await waitFor(() => expect(statusMock).toHaveBeenCalledTimes(1));
    const signal = statusMock.mock.calls[0]![1]!.signal as AbortSignal;
    await unmount();
    expect(signal.aborted).toBe(true);
  });
});
