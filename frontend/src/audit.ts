import type {
  AuditAttachmentState,
  AuditEvent,
  AuditPathState,
} from "./api.js";

const eventLabels: Record<string, string> = {
  audit_enroll: "Audit protection enabled",
  audit_inherit: "Audit protection inherited",
  content_create: "Content created",
  content_replace: "Content replaced",
  content_revert: "Earlier content restored",
  node_create: "Document created",
  node_path: "Path changed",
  provenance_add: "Provenance recorded",
  tag_assign: "Tag assigned",
  tag_delete: "Tag deleted",
  tag_rename: "Tag renamed",
  tag_unassign: "Tag removed",
};

export function auditEventLabel(kind: string): string {
  return eventLabels[kind] ?? kind.replaceAll("_", " ");
}

export function auditEventSummary(event: AuditEvent): string {
  if (event.old_path && event.new_path) {
    return `${auditPathLabel(event.old_path)} → ${auditPathLabel(event.new_path)}`;
  }
  if (
    event.prior_current_version_id !== undefined ||
    event.resulting_current_version_id !== undefined
  ) {
    return `Version ${shortID(event.prior_current_version_id)} → ${shortID(event.resulting_current_version_id)}`;
  }
  if (event.attachment) {
    switch (event.attachment.kind) {
      case "tag_definition":
        return `${tagName(event.attachment.before)} → ${tagName(event.attachment.after)}`;
      case "tag_assignment":
        return `Tag ${shortID(event.attachment.identity.tag_id)} on id:${event.attachment.identity.node_id ?? event.node_id}`;
      case "provenance":
        return provenanceSummary(event.attachment.after ?? event.attachment.before);
    }
  }
  return `Revision ${event.prior_node_revision} → ${event.resulting_node_revision}`;
}

export function auditPathLabel(path: AuditPathState): string {
  return path.state === "trash" ? `${path.path} (trash)` : path.path;
}

export function shortID(value: string | undefined, length = 8): string {
  if (!value) return "none";
  return value.length > length ? value.slice(0, length) : value;
}

function tagName(state: AuditAttachmentState | undefined): string {
  return state?.tag_name || "(absent)";
}

function provenanceSummary(state: AuditAttachmentState | undefined): string {
  if (!state) return "Provenance removed";
  return state.original_path || `Ingest ${shortID(state.ingest_id)}`;
}
