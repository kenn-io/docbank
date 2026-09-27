import { getProductionDraftAt, type ProductionDraft, type ProductionMember,
  type ProductionPreparedMember } from "./api.js";

const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const shaPattern = /^[0-9a-f]{64}$/;
const relationPattern = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/;

export class StaleMemberAppendError extends Error {
  constructor() { super("The production draft changed while preparing the new member."); }
}

function validSource(source: ProductionPreparedMember): boolean {
  const family = source.family;
  if (!family || !uuidPattern.test(source.id) || !uuidPattern.test(source.vault_id) ||
      !uuidPattern.test(source.source_version_id) || !uuidPattern.test(family.root_version_id) ||
      !Number.isSafeInteger(source.node_id) || source.node_id < 1 ||
      !Number.isSafeInteger(source.source_size) || source.source_size < 0 ||
      !Number.isSafeInteger(source.pdf_size) || source.pdf_size < 1 ||
      !shaPattern.test(source.source_sha256) || !shaPattern.test(source.pdf_sha256) ||
      !shaPattern.test(source.map_sha256) || !shaPattern.test(source.page_inventory_sha256) ||
      !["keep_selected", "redact_selected"].includes(source.mode)) return false;
  if (family.kind === "email_attachment") {
    return relationPattern.test(family.relation_operation_id ?? "") &&
      Number.isSafeInteger(family.relation_order) && (family.relation_order ?? 0) > 0;
  }
  return ["standalone", "email_message", "transcript"].includes(family.kind) &&
    family.root_version_id === source.source_version_id && !family.relation_operation_id && !family.relation_order;
}

/** Copy exact prepared source authority into a new, unreviewed occurrence. */
export async function loadCurrentMemberAppend(session: string, draft: ProductionDraft, targetMembers: ProductionMember[],
  nextCursor: string, source: ProductionPreparedMember, memberID: string,
  signal: AbortSignal): Promise<ProductionPreparedMember> {
  if (nextCursor) throw new Error("Load all target members before adding another occurrence.");
  if (targetMembers.length >= 100_000 || !shaPattern.test(draft.member_hash) ||
      draft.state !== "draft" || draft.membership_sealed) throw new Error("This draft cannot accept another member.");
  const ids = new Set<string>();
  for (const [index, member] of targetMembers.entries()) {
    if (member.ordinal !== index + 1 || ids.has(member.id)) throw new Error("Target members must be ordered without duplicates.");
    ids.add(member.id);
  }
  if (!uuidPattern.test(memberID) || ids.has(memberID) || !validSource(source)) {
    throw new Error("The prepared source identity is incomplete or invalid.");
  }
  const fresh = await getProductionDraftAt(session, draft.set_id, draft.revision, signal);
  signal.throwIfAborted();
  if (fresh.set_id !== draft.set_id || fresh.revision !== draft.revision || fresh.etag !== draft.etag ||
      fresh.member_hash !== draft.member_hash || fresh.state !== "draft" || fresh.membership_sealed) {
    throw new StaleMemberAppendError();
  }
  return {
    id: memberID, vault_id: source.vault_id, node_id: source.node_id,
    source_version_id: source.source_version_id, source_sha256: source.source_sha256,
    source_size: source.source_size, pdf_sha256: source.pdf_sha256, pdf_size: source.pdf_size,
    map_sha256: source.map_sha256, page_inventory_sha256: source.page_inventory_sha256,
    family: { ...source.family }, mode: source.mode, ordinal: targetMembers.length + 1,
    reviewed: false, review_binding: "",
  };
}
