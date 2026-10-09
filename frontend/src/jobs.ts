import type { Job, LaneControl } from "./generated/docbank.js";
import { timestampComparable } from "./format.js";

const activeJob = (job: Job): boolean => job.status === "running" || job.status === "queued";

export function laneTitle(lane: string): string {
  const titles: Record<string, string> = { photo_import: "Photo import", place: "Storage placement", evacuate: "Storage evacuation", repair: "Storage repair", salvage: "Storage salvage", "derive:visual-previews": "Visual previews", "storage:pack": "Automatic packing" };
  return titles[lane] ?? lane.replaceAll(":", " · ").replaceAll("-", " ").replaceAll("_", " ");
}

export function jobLanes(jobs: Job[], controls: LaneControl[] = []) {
  const groups = new Map<string, { lane: string; members: Job[] }>();
  for (const job of jobs) {
    const lane = job.operation_id ? job.kind! : job.name;
    const group = groups.get(lane) ?? { lane, members: [] };
    group.members.push(job);
    groups.set(lane, group);
  }
  for (const control of controls) {
    if (!groups.has(control.lane)) groups.set(control.lane, { lane: control.lane, members: [] });
  }
  return Array.from(groups, ([key, { lane, members }]) => {
    members.sort((a, b) => timestampComparable(b.started_at).localeCompare(timestampComparable(a.started_at)) || b.name.localeCompare(a.name));
    const active = members.filter(activeJob);
    const job = active.find((member) => member.status === "running") ?? active[0] ?? members[0];
    const progressJobs = active.length ? active : members.slice(0, 1);
    const known = progressJobs.length > 0 && progressJobs.every((member) => member.operation_id && member.total_objects !== undefined && member.total_objects > 0);
    return {
      key, members, lane, title: laneTitle(lane), control: controls.find((control) => control.lane === lane),
      status: active.some((member) => member.status === "running") ? "running" as const : job?.status,
      completed: progressJobs.reduce((sum, member) => sum + (member.completed_objects ?? 0), 0),
      total: known ? progressJobs.reduce((sum, member) => sum + member.total_objects!, 0) : undefined,
      active: active.length > 0,
    };
  }).sort((a, b) => a.lane.localeCompare(b.lane));
}
