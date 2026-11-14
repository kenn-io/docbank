import { sessionJSON } from "./api-transport.js";
import { decodeBatesExport } from "./bates.js";
import { pageDigest } from "./pages.js";
import {
  requireRedaction as check,
  redactionObject as object,
  redactionInteger as integer,
  redactionHash as hash,
} from "./redaction.js";
import type {
  BatesNamespace,
  BatesNamespaceDetail,
  BatesExport,
  RedactionPreparation,
  RedactionMask,
  RedactionReview,
  NumberedReservation,
} from "./generated/docbank.js";

export interface NumberingInputs {
  preparation: RedactionPreparation;
  masks: RedactionMask[];
  review: RedactionReview;
}
export interface FrozenNumbering extends NumberingInputs {
  namespace: BatesNamespace;
  start: number;
}
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const same = (a: unknown, b: unknown) => pageDigest(a) === pageDigest(b);
const responseLimit = 256 << 10;

export function numberedLabels(ns: BatesNamespace, start: number, pages: number): string[] {
  if (
    !Number.isSafeInteger(start) ||
    start < 1 ||
    !Number.isSafeInteger(pages) ||
    pages < 1 ||
    start + pages > 10 ** ns.padding
  )
    return [];
  return Array.from(
    { length: pages },
    (_, i) => ns.prefix + String(start + i).padStart(ns.padding, "0") + ns.suffix,
  );
}

export async function readBatesNamespaceDetail(
  session: string,
  id: string,
  signal?: AbortSignal,
): Promise<BatesNamespaceDetail> {
  check(uuid.test(id));
  const value = await sessionJSON<unknown>(`/api/v1/bates/namespaces/${id}`, {
    session,
    signal,
    responseLimit,
  });
  const o = object(value, ["namespace", "next_sequence"], ["$schema"]);
  const n = object(o.namespace, ["namespace_id", "prefix", "suffix", "padding", "created_at"]);
  check(
    n.namespace_id === id &&
      typeof n.prefix === "string" &&
      typeof n.suffix === "string" &&
      typeof n.created_at === "string" &&
      Number.isFinite(Date.parse(n.created_at)),
  );
  integer(n.padding, 1, 10);
  integer(o.next_sequence, 1, 10 ** (n.padding as number));
  return o as unknown as BatesNamespaceDetail;
}

export function decodeNumberedReservation(
  value: unknown,
  frozen: FrozenNumbering,
): NumberedReservation {
  const o = object(
    value,
    [
      "contract",
      "recipe",
      "recipe_sha256",
      "allocation_id",
      "snapshot_id",
      "request_sha256",
      "state",
      "pages",
    ],
    ["$schema"],
  );
  check(
    o.contract === "native-pdf-numbered-reservation/v1" &&
      typeof o.allocation_id === "string" &&
      uuid.test(o.allocation_id) &&
      typeof o.snapshot_id === "string" &&
      uuid.test(o.snapshot_id) &&
      ["reserved", "committed"].includes(String(o.state)),
  );
  const r = object(o.recipe, [
    "contract",
    "vault_uid",
    "selection",
    "preparation_sha256",
    "decisions_sha256",
    "review_sha256",
    "resolved_sha256",
    "renderer",
    "namespace",
    "page_count",
    "pages",
    "footer",
    "original_retention",
    "original_release_available",
  ]);
  const review = frozen.review,
    ns = frozen.namespace;
  check(
    r.contract === "native-pdf-numbered/v1" &&
      typeof r.vault_uid === "string" &&
      uuid.test(r.vault_uid),
  );
  check(
    same(r.selection, review.selection) &&
      r.preparation_sha256 === review.preparation_sha256 &&
      r.decisions_sha256 === review.decisions_sha256 &&
      r.review_sha256 === review.sha256 &&
      r.resolved_sha256 === review.resolved.sha256 &&
      same(r.renderer, review.recipe) &&
      r.page_count === review.resolved.page_count &&
      same(r.pages, review.resolved.pages),
  );
  check(
    same(r.namespace, {
      id: ns.namespace_id,
      prefix: ns.prefix,
      suffix: ns.suffix,
      padding: ns.padding,
      start: frozen.start,
    }),
  );
  check(r.original_retention === "indefinite" && r.original_release_available === false);
  check(
    same(r.footer, {
      strip_height: 5000,
      inset_x: 2000,
      inset_top: 1000,
      inset_bottom: 1000,
      font_sha256: review.recipe.font_sha256,
      font_size_milli_points: 10000,
    }),
  );
  check(hash(o.recipe_sha256) === pageDigest(r));
  const occurrence = pageDigest({
    vault_uid: r.vault_uid,
    node_id: review.selection.node_id,
    version_id: review.selection.source.version_id,
  }).slice(0, 32);
  const labels = numberedLabels(ns, frozen.start, review.resolved.page_count);
  check(labels.length === review.resolved.page_count);
  const pages = labels.map((label, i) => ({
    ordinal: i + 1,
    occurrence_id: occurrence,
    source_blob_sha256: review.selection.source.sha256,
    source_page: i + 1,
    output_page: i + 1,
    label,
  }));
  check(same(o.pages, pages));
  check(
    hash(o.request_sha256) ===
      pageDigest({
        namespace_id: ns.namespace_id,
        snapshot_id: o.snapshot_id,
        recipe_sha256: o.recipe_sha256,
        start_at: frozen.start,
        // The durable ledger digest predates the transport DTOs and uses Go field names.
        pages: pages.map((p) => ({
          OccurrenceID: occurrence,
          UnstampedSHA256: p.source_blob_sha256,
          SourcePage: p.source_page,
          VerifiedPageCount: pages.length,
        })),
      }),
  );
  return o as unknown as NumberedReservation;
}

export async function reserveNumberedRedaction(
  session: string,
  frozen: FrozenNumbering,
  signal?: AbortSignal,
): Promise<NumberedReservation> {
  const { preparation, masks, review, namespace, start } = frozen;
  const value = await sessionJSON<unknown>("/api/v1/redaction/reservations", {
    session,
    signal,
    responseLimit,
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      preparation,
      masks,
      review_sha256: review.sha256,
      namespace_id: namespace.namespace_id,
      start_at: start,
      retain_original: true,
    }),
  });
  return decodeNumberedReservation(value, frozen);
}

export function decodeNumberedPublication(
  value: unknown,
  frozen: FrozenNumbering,
  reservation: NumberedReservation,
): BatesExport {
  const r = decodeBatesExport(value),
    a = r.allocation,
    n = r.namespace;
  check(
    r.artifact_id === reservation.allocation_id &&
      r.allocation_id === reservation.allocation_id &&
      r.recipe_sha256 === reservation.recipe_sha256 &&
      same(r.recipe, reservation.recipe) &&
      same(r.pages, reservation.pages) &&
      r.page_count === reservation.pages.length &&
      r.size <= 512 << 20,
  );
  check(same(n, frozen.namespace));
  check(
    a &&
      a.allocation_id === reservation.allocation_id &&
      a.snapshot_id === reservation.snapshot_id &&
      a.namespace_id === frozen.namespace.namespace_id &&
      a.state === "committed" &&
      a.recipe_sha256 === reservation.recipe_sha256 &&
      a.request_sha256 === reservation.request_sha256 &&
      a.start_sequence === frozen.start &&
      a.end_sequence === frozen.start + reservation.pages.length - 1 &&
      same(
        a.labels,
        reservation.pages.map((p) => ({
          ordinal: p.ordinal,
          occurrence_id: p.occurrence_id,
          source_page: p.source_page,
          output_page: p.output_page,
          label: p.label,
        })),
      ),
  );
  check(
    r.manifest_sha256 ===
      pageDigest({
        contract: "bates-artifact/v1",
        allocation_id: r.allocation_id,
        artifact_id: r.artifact_id,
        blob_sha256: r.blob_sha256,
        size: r.size,
        media_type: r.media_type,
        page_count: r.page_count,
        recipe_sha256: r.recipe_sha256,
        pages: r.pages,
      }),
  );
  return r;
}
export async function publishNumberedRedaction(
  session: string,
  frozen: FrozenNumbering,
  reservation: NumberedReservation,
  signal?: AbortSignal,
): Promise<BatesExport> {
  const value = await sessionJSON<unknown>("/api/v1/redaction/publications", {
    session,
    signal,
    responseLimit,
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      preparation: frozen.preparation,
      masks: frozen.masks,
      review_sha256: frozen.review.sha256,
      allocation_id: reservation.allocation_id,
    }),
  });
  return decodeNumberedPublication(value, frozen, reservation);
}
