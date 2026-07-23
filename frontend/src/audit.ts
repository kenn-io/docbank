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
  provenance_supersede: "Provenance superseded",
  tag_assign: "Tag assigned",
  tag_define: "Tag defined",
  tag_delete: "Tag deleted",
  tag_rename: "Tag renamed",
  tag_unassign: "Tag removed",
};

export function auditEventLabel(kind: string): string {
  return eventLabels[kind] ?? kind.replaceAll("_", " ");
}

export function auditEventSummary(event: AuditEvent): string {
  switch (event.kind) {
    case "node_path":
      if (event.old_path && event.new_path) {
        return `${auditPathLabel(event.old_path)} → ${auditPathLabel(event.new_path)}`;
      }
      break;
    case "content_create":
    case "content_replace":
    case "content_revert":
      return `Version ${shortID(event.prior_current_version_id)} → ${shortID(event.resulting_current_version_id)}`;
    case "tag_assign":
    case "tag_define":
    case "tag_delete":
    case "tag_rename":
    case "tag_unassign":
    case "provenance_add":
    case "provenance_supersede":
      if (event.attachment) return attachmentSummary(event);
      break;
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

function attachmentSummary(event: AuditEvent): string {
  const change = event.attachment;
  if (!change) return `Revision ${event.prior_node_revision} → ${event.resulting_node_revision}`;
  switch (change.kind) {
    case "tag_definition":
      return `${tagName(change.before)} → ${tagName(change.after)}`;
    case "tag_assignment":
      return `Tag ${shortID(change.identity.tag_id)} on id:${change.identity.node_id ?? event.node_id}`;
    case "provenance":
      return provenanceSummary(change.after ?? change.before);
  }
}
