import { describe, expect, it } from "vitest";
import {
  auditEventLabel,
  auditEventSummary,
  auditPathLabel,
} from "./audit.js";
import type { AuditEvent } from "./api.js";

function event(overrides: Partial<AuditEvent> = {}): AuditEvent {
  return {
    id: "a".repeat(64),
    operation_id: "11111111-1111-4111-8111-111111111111",
    operation_sequence: 2,
    ordinal: 0,
    node_id: 42,
    kind: "content_replace",
    scope_id: "22222222-2222-4222-8222-222222222222",
    recorded_at: "2026-07-23T12:00:00Z",
    origin: "api",
    prior_node_revision: 1,
    resulting_node_revision: 2,
    ...overrides,
  };
}

describe("audit presentation", () => {
  it("uses human event names without hiding unknown canonical kinds", () => {
    expect(auditEventLabel("content_replace")).toBe("Content replaced");
    expect(auditEventLabel("future_event")).toBe("future event");
  });

  it("preserves live and retained-trash coordinates in path summaries", () => {
    const summary = auditEventSummary(
      event({
        kind: "node_path",
        old_path: { path: "/Taxes/return.pdf", state: "live" },
        new_path: { path: "@trash/known/Taxes/return.pdf", state: "trash" },
      }),
    );
    expect(summary).toBe(
      "/Taxes/return.pdf → @trash/known/Taxes/return.pdf (trash)",
    );
    expect(auditPathLabel({ path: "/Taxes", state: "live" })).toBe("/Taxes");
  });

  it("summarizes version and typed attachment changes", () => {
    expect(
      auditEventSummary(
        event({
          prior_current_version_id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
          resulting_current_version_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
        }),
      ),
    ).toBe("Version aaaaaaaa → bbbbbbbb");

    expect(
      auditEventSummary(
        event({
          kind: "tag_rename",
          prior_current_version_id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
          resulting_current_version_id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
          attachment: {
            kind: "tag_definition",
            identity: { tag_id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc" },
            before: { tag_name: "draft" },
            after: { tag_name: "filed" },
          },
        }),
      ),
    ).toBe("draft → filed");

    expect(
      auditEventSummary(
        event({
          kind: "provenance_add",
          prior_current_version_id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
          resulting_current_version_id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
          attachment: {
            kind: "provenance",
            identity: { provenance_id: "d".repeat(64) },
            after: {
              ingest_id: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
              original_path: "records/return.pdf",
            },
          },
        }),
      ),
    ).toBe("records/return.pdf");

    expect(
      auditEventSummary(
        event({
          kind: "tag_define",
          attachment: {
            kind: "tag_definition",
            identity: { tag_id: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee" },
            after: { tag_name: "tax" },
          },
        }),
      ),
    ).toBe("(absent) → tax");

    expect(
      auditEventSummary(
        event({
          kind: "provenance_supersede",
          attachment: {
            kind: "provenance",
            identity: { provenance_id: "f".repeat(64) },
            before: {
              ingest_id: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
              original_path: "records/draft.pdf",
            },
            after: {
              ingest_id: "ffffffff-ffff-4fff-8fff-ffffffffffff",
              original_path: "records/final.pdf",
            },
          },
        }),
      ),
    ).toBe("records/final.pdf");
  });
});
