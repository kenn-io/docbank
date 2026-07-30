import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/svelte";
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

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("audit evidence drawer", () => {
  it("shows independently verified authority and protected content", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
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

    expect(
      await screen.findByText("Protected history and content agree"),
    ).toBeTruthy();
    expect(screen.getByText("3 of 3 unique retained blobs passed SHA-256 verification.")).toBeTruthy();
    expect(screen.getByText(evidence.lineage_id)).toBeTruthy();
    expect(screen.getByText(evidence.scopes[0]!.id)).toBeTruthy();
    expect(screen.getByText(evidence.scopes[0]!.chain_head)).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/v1/audit/verify",
      expect.objectContaining({
        method: "POST",
        body: "{}",
        credentials: "same-origin",
      }),
    );
  });

  it("distinguishes a dormant vault and can retry verification", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            enabled: false,
            protected_blobs: 0,
            protected_bytes: 0,
            verified_blobs: 0,
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

    expect(await screen.findByText("Permanent audit is dormant")).toBeTruthy();
    await fireEvent.click(
      screen.getByRole("button", {
        name: "Run permanent audit verification again",
      }),
    );

    expect(await screen.findByText("Protected authority needs attention")).toBeTruthy();
    expect(screen.getByText("corrupt")).toBeTruthy();
    expect(screen.getByText("c".repeat(64))).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("reports metadata replay failures instead of calling the vault dormant", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          enabled: false,
          protected_blobs: 0,
          protected_bytes: 0,
          verified_blobs: 0,
          metadata_problems: ["audit scope chain does not match its head"],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );

    render(AuditEvidenceDrawer, {
      session: "short-lived",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(
      await screen.findByText("Protected authority needs attention"),
    ).toBeTruthy();
    expect(
      screen.getByText("audit scope chain does not match its head"),
    ).toBeTruthy();
    expect(screen.queryByText("Permanent audit is dormant")).toBeNull();
  });

  it("withdraws an earlier verified result while refresh is unresolved", async () => {
    let rejectRefresh!: (cause: Error) => void;
    const refreshResponse = new Promise<Response>((_resolve, reject) => {
      rejectRefresh = reject;
    });
    vi.spyOn(globalThis, "fetch")
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

    expect(
      await screen.findByText("Protected history and content agree"),
    ).toBeTruthy();
    await fireEvent.click(
      screen.getByRole("button", {
        name: "Run permanent audit verification again",
      }),
    );

    expect(
      screen.getByText(
        "Replaying protected history and hashing retained content…",
      ),
    ).toBeTruthy();
    expect(
      screen.queryByText("Protected history and content agree"),
    ).toBeNull();

    rejectRefresh(new Error("verification interrupted"));
    expect((await screen.findByRole("alert")).textContent).toContain(
      "verification interrupted",
    );
    expect(
      screen.queryByText("Protected history and content agree"),
    ).toBeNull();
  });
});
