import { describe, expect, it } from "vite-plus/test";
import type { Job } from "./generated/docbank.js";
import { jobLanes } from "./jobs.js";

const job = (fields: Partial<Job>): Job => ({
  name: "storage:a",
  operation_id: "a",
  kind: "place",
  lane: "place",
  status: "running",
  busy: true,
  started_at: "2026-07-23T12:00:00Z",
  controllable: true,
  paused: false,
  can_set_concurrency: false,
  ...fields,
});

describe("activity lanes", () => {
  it("compares UTC timestamps at full fractional precision and retains paused idle lanes", () => {
    const [lane] = jobLanes(
      [
        job({ status: "failed", busy: false, started_at: "2026-07-23T12:00:00.1Z" }),
        job({
          name: "storage:b",
          operation_id: "b",
          status: "completed",
          busy: false,
          started_at: "2026-07-23T12:00:00.12Z",
        }),
        job({
          name: "storage:c",
          operation_id: "c",
          status: "cancelled",
          busy: false,
          started_at: "2026-07-23T12:00:00.120000001Z",
        }),
      ],
      [{ lane: "place", paused: true, concurrency: 1, revision: 2, can_set_concurrency: false }],
    );
    expect(lane.control?.paused).toBe(true);
    expect(lane.status).toBe("failed");
    expect(lane.visible).toBe(true);
    expect(lane.members.map((member) => member.operation_id)).toEqual(["c", "b", "a"]);
    const controls = [
      { lane: "repair", paused: false, concurrency: 1, revision: 1, can_set_concurrency: false },
      { lane: "place", paused: true, concurrency: 1, revision: 4, can_set_concurrency: false },
    ];
    const idle = jobLanes([], controls);
    expect(idle).toHaveLength(2);
    expect(idle[0]).toMatchObject({ lane: "place", members: [], status: undefined });
    expect(idle[1]).toMatchObject({ lane: "repair", members: [], control: { paused: false } });
    const after = jobLanes([job({ kind: "repair", lane: "repair" })], controls);
    expect(after.map(({ lane, key }) => [lane, key])).toEqual([
      ["place", "place"],
      ["repair", "repair"],
    ]);
    expect(after.map(({ key }) => key)).toEqual(idle.map(({ key }) => key));
  });

  it("groups by API lane, summing only active progress", () => {
    const control = {
      lane: "photo_checkout",
      paused: true,
      concurrency: 1,
      revision: 2,
      can_set_concurrency: false,
    };
    const lanes = jobLanes(
      [
        job({ total_objects: 5, completed_objects: 2 }),
        job({
          name: "storage:b",
          operation_id: "b",
          status: "queued",
          total_objects: 7,
          completed_objects: 1,
        }),
        job({
          name: "storage:c",
          operation_id: "c",
          status: "completed",
          busy: false,
          total_objects: 99,
          completed_objects: 99,
        }),
        job({
          name: "storage:checkout",
          operation_id: "checkout",
          kind: "photo_checkout",
          lane: "photo_checkout",
          status: "completed",
          busy: false,
          total_objects: 2,
          completed_objects: 1,
        }),
        job({
          name: "storage:commit",
          kind: "photo_checkout_commit",
          lane: "photo_checkout",
          status: "queued",
          completed_objects: 1,
          total_objects: 3,
        }),
        job({ name: "watch:inbox", lane: "watch:inbox", operation_id: undefined, kind: undefined }),
      ],
      [control],
    );
    expect(lanes).toHaveLength(3);
    expect(lanes.find(({ lane }) => lane === "place")).toMatchObject({
      lane: "place",
      completed: 3,
      total: 12,
      status: "running",
      active: true,
    });
    expect(lanes.find(({ lane }) => lane === "photo_checkout")).toMatchObject({
      title: "Photo checkout",
      completed: 1,
      total: 3,
      active: true,
      control,
    });
    expect(
      lanes.find(({ lane }) => lane === "photo_checkout")?.members.map((member) => member.kind),
    ).toContain("photo_checkout_commit");
    expect(lanes.find(({ lane }) => lane === "watch:inbox")?.key).toBe("watch:inbox");
    const [unknown] = jobLanes([
      job({ total_objects: 5 }),
      job({ name: "storage:b", operation_id: "b", total_objects: 0 }),
    ]);
    expect(unknown.total).toBeUndefined();
  });
});
