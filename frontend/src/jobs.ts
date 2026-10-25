import type { Job, LaneControl } from "./generated/docbank.js";
import { timestampComparable } from "./format.js";

const activeJob = (job: Job): boolean => job.busy || job.status === "queued";

export function laneTitle(lane: string): string {
  const titles: Record<string, string> = {
    photo_import: "Importing photos",
    photo_checkout: "Photo checkout",
    photo_checkout_commit: "Saving changes",
    photo_checkout_scan: "Checking files",
    photo_checkout_rebind: "Changing folder",
    place: "Moving stored files",
    evacuate: "Emptying storage",
    repair: "Repairing storage",
    salvage: "Recovering files",
    "extract:plain-text": "Extracting text",
    "extract:source-metadata": "Reading file metadata",
    "extract:photo-sidecars": "Reading photo sidecars",
    "extract:email": "Reading email",
    "derive:visual-previews": "Making previews",
    "derive:photo-quality": "Assessing photos",
    "derive:document-events": "Finding document dates",
    "derive:document-people": "Finding people",
    "storage:pack": "Packing storage",
    "import:packages": "Importing packages",
    "exports:write": "Exporting files",
    "pages:render": "Rendering pages",
    "process:renditions": "Processing documents",
    "process:embeddings": "Preparing similarity search",
    "process:vector-indexes": "Updating similarity search",
    "process:media-continuations": "Processing media",
    "process:photo-descriptions": "Describing photos",
    "probe:media-origins": "Checking media sources",
    "maintenance:auxiliary-checksums": "Checking file hashes",
    "maintenance:retention": "Cleaning up old jobs",
    "daemon.idle-timeout": "Idle shutdown timer",
    "telemetry:heartbeat": "Usage reporting",
  };
  if (lane.startsWith("import:mailbox:")) return `Importing mailbox ${lane.split(":").at(-1)}`;
  if (lane.startsWith("watch:")) return `Watching ${lane.slice(6)}`;
  if (lane.startsWith("process:renditions:"))
    return `Processing documents ${lane.split(":").at(-1)}`;
  if (lane.startsWith("checkout:startup:")) return "Checking checkout folder";
  return titles[lane] ?? lane.replaceAll(":", " ").replaceAll("-", " ").replaceAll("_", " ");
}

export function jobLanes(jobs: Job[], controls: LaneControl[] = []) {
  const groups = new Map<string, { lane: string; members: Job[] }>();
  for (const job of jobs) {
    const lane = job.lane;
    const group = groups.get(lane) ?? { lane, members: [] };
    group.members.push(job);
    groups.set(lane, group);
  }
  for (const control of controls) {
    if (!groups.has(control.lane)) groups.set(control.lane, { lane: control.lane, members: [] });
  }
  return Array.from(groups, ([key, { lane, members }]) => {
    members.sort(
      (a, b) =>
        timestampComparable(b.started_at).localeCompare(timestampComparable(a.started_at)) ||
        b.name.localeCompare(a.name),
    );
    const active = members.filter(activeJob);
    const failed = members.find((member) => member.status === "failed" || member.error);
    const job = active.find((member) => member.busy) ?? active[0] ?? failed ?? members[0];
    const progressJobs = active.length ? active : members.slice(0, 1);
    const known =
      progressJobs.length > 0 &&
      progressJobs.every(
        (member) =>
          member.operation_id && member.total_objects !== undefined && member.total_objects > 0,
      );
    return {
      key,
      members,
      lane,
      title: laneTitle(lane),
      control: controls.find((control) => control.lane === lane),
      status: active.some((member) => member.busy) ? ("running" as const) : job?.status,
      completed: progressJobs.reduce((sum, member) => sum + (member.completed_objects ?? 0), 0),
      total: known
        ? progressJobs.reduce((sum, member) => sum + member.total_objects!, 0)
        : undefined,
      active: members.some((member) => member.busy),
      failed: failed !== undefined,
      visible:
        active.length > 0 ||
        failed !== undefined ||
        controls.some((control) => control.lane === lane && control.paused),
    };
  }).sort((a, b) => a.lane.localeCompare(b.lane));
}
