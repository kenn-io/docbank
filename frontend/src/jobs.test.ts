import { describe, expect, it } from "vitest";
import type { Job } from "./generated/docbank.js";
import { jobLanes } from "./jobs.js";

const job = (fields: Partial<Job>): Job => ({ name: "storage:a", operation_id: "a", kind: "place", status: "running", started_at: "2026-07-23T12:00:00Z", controllable: true, paused: false, can_set_concurrency: false, ...fields });

describe("activity lanes", () => {
  it("compares UTC timestamps at full fractional precision and retains paused idle lanes", () => {
    const [lane] = jobLanes([
      job({ status: "failed", started_at: "2026-07-23T12:00:00.1Z" }),
      job({ name: "storage:b", operation_id: "b", status: "completed", started_at: "2026-07-23T12:00:00.12Z" }),
      job({ name: "storage:c", operation_id: "c", status: "cancelled", started_at: "2026-07-23T12:00:00.120000001Z" }),
    ], [{ lane: "place", paused: true, concurrency: 1, revision: 2, can_set_concurrency: false }]);
    expect(lane.control?.paused).toBe(true);
    expect(lane.status).toBe("cancelled");
    expect(lane.members.map((member) => member.operation_id)).toEqual(["c", "b", "a"]);
    const controls = [
      { lane: "repair", paused: false, concurrency: 1, revision: 1, can_set_concurrency: false },
      { lane: "place", paused: true, concurrency: 1, revision: 4, can_set_concurrency: false },
    ];
    const idle = jobLanes([], controls);
    expect(idle).toHaveLength(2);
    expect(idle[0]).toMatchObject({ lane: "place", members: [], status: undefined });
    expect(idle[1]).toMatchObject({ lane: "repair", members: [], control: { paused: false } });
    const after = jobLanes([job({ kind: "repair" })], controls);
    expect(after.map(({ lane, key }) => [lane, key])).toEqual([["place", "place"], ["repair", "repair"]]);
    expect(after.map(({ key }) => key)).toEqual(idle.map(({ key }) => key));
  });

  it("groups by storage kind and worker name, summing only active progress", () => {
    const lanes = jobLanes([
      job({ total_objects: 5, completed_objects: 2 }),
      job({ name: "storage:b", operation_id: "b", status: "queued", total_objects: 7, completed_objects: 1 }),
      job({ name: "storage:c", operation_id: "c", status: "completed", total_objects: 99, completed_objects: 99 }),
      job({ name: "watch:inbox", operation_id: undefined, kind: undefined }),
    ]);
    expect(lanes).toHaveLength(2);
    expect(lanes[0]).toMatchObject({ lane: "place", completed: 3, total: 12, status: "running" });
    expect(lanes[1].key).toBe("watch:inbox");
    const [unknown] = jobLanes([job({ total_objects: 5 }), job({ name: "storage:b", operation_id: "b", total_objects: 0 })]);
    expect(unknown.total).toBeUndefined();
  });
});
