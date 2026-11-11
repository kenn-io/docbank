import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";
import { buildRecipientPackage, discardRecipientPackages } from "./generated/docbank.js";
import type { BatesExport, RecipientPackageDownload } from "./generated/docbank.js";
import type { NumberingInputs } from "./redactionNumbering.js";
import {
  requireRedaction as check,
  redactionObject as object,
  redactionInteger as integer,
  redactionHash as hash,
} from "./redaction.js";
import { pageDigest } from "./pages.js";

const root = "/api/daemon/web-download/file";
const metadataLimit = 256 << 10;

export function recipientText(inputs: NumberingInputs): { size: number; sha256: string } {
  const plan = inputs.review.resolved;
  integer(plan.page_count, 1, 16);
  const chunks: string[] = [];
  let page = 1,
    size = plan.page_count;
  const encoder = new TextEncoder();
  for (const run of plan.runs) {
    integer(run.page, page, plan.page_count);
    check(typeof run.text === "string");
    size += encoder.encode(run.text).length;
    check(size <= 16 << 20);
    while (page < run.page) {
      chunks.push("\f");
      page++;
    }
    chunks.push(run.text);
  }
  while (page++ <= plan.page_count) chunks.push("\f");
  return { size, sha256: bytesToHex(sha256(encoder.encode(chunks.join("")))) };
}

export function decodeRecipientPackage(
  value: unknown,
  inputs: NumberingInputs,
  artifact: BatesExport,
): RecipientPackageDownload {
  const o = object(
    value,
    [
      "contract",
      "id",
      "allocation_id",
      "receipt",
      "size",
      "sha256",
      "expires_at",
      "url",
      "name",
      "verification",
    ],
    ["$schema"],
  );
  check(
    o.contract === "native-pdf-recipient-download/v1" &&
      o.verification === "masks_text_and_numbers" &&
      o.allocation_id === artifact.allocation_id &&
      o.name === "recipient.zip" &&
      typeof o.id === "string" &&
      /^[0-9a-f]{48}$/.test(o.id) &&
      typeof o.expires_at === "string" &&
      Number.isFinite(Date.parse(o.expires_at)),
  );
  integer(o.size, 1, 512 << 20);
  hash(o.sha256);
  check(typeof o.url === "string" && o.url.startsWith(root + "?"));
  const url = new URL(o.url, "https://download.invalid");
  const ticket = url.searchParams.get("ticket");
  check(
    url.pathname === root &&
      !url.hash &&
      url.searchParams.size === 1 &&
      ticket !== null &&
      /^[A-Za-z0-9_-]{43}$/.test(ticket) &&
      o.url === root + "?ticket=" + ticket,
  );
  const receipt = object(o.receipt, [
    "contract",
    "document_count",
    "page_count",
    "dat_profile",
    "opt_profile",
    "load_file",
    "page_map",
    "mapping",
    "documents",
  ]);
  check(
    receipt.contract === "native-pdf-recipient-package/v1" &&
      receipt.document_count === 1 &&
      receipt.page_count === artifact.page_count &&
      Array.isArray(receipt.documents) &&
      receipt.documents.length === 1,
  );
  const d = object(receipt.documents[0], [
    "ordinal",
    "document_id",
    "allocation_id",
    "namespace_id",
    "artifact_manifest_sha256",
    "recipe_sha256",
    "review_sha256",
    "page_count",
    "labels",
    "pdf",
    "text",
  ]);
  const labels = artifact.pages.map((p) => p.label);
  check(
    artifact.namespace &&
      d.ordinal === 1 &&
      d.document_id === labels[0] &&
      d.allocation_id === artifact.allocation_id &&
      d.namespace_id === artifact.namespace.namespace_id &&
      d.artifact_manifest_sha256 === artifact.manifest_sha256 &&
      d.recipe_sha256 === artifact.recipe_sha256 &&
      d.review_sha256 === inputs.review.sha256 &&
      d.page_count === artifact.page_count &&
      pageDigest(d.labels) === pageDigest(labels),
  );
  for (const label of labels)
    check(new TextEncoder().encode(label).length <= 256 && !/[,\r\n]/.test(label));
  const file = (value: unknown, path: string, max: number) => {
    const f = object(value, ["path", "size", "sha256"]);
    check(f.path === path);
    integer(f.size, 1, max);
    hash(f.sha256);
    return f;
  };
  const pdf = file(d.pdf, "VOL001/PDF/000001.pdf", 512 << 20);
  check(pdf.size === artifact.size && pdf.sha256 === artifact.blob_sha256);
  const text = file(d.text, "VOL001/TEXT/000001.txt", 16 << 20),
    expected = recipientText(inputs);
  check(text.size === expected.size && text.sha256 === expected.sha256);
  file(receipt.load_file, "VOL001/DATA.DAT", metadataLimit);
  file(receipt.page_map, "VOL001/DATA.OPT", metadataLimit);
  file(receipt.mapping, "mapping.json", metadataLimit);
  const profileKeys = [
    "id",
    "field",
    "qualifier",
    "newline_in_field",
    "header_row",
    "columns",
    "encoding",
    "date_format",
    "declared_timezone",
  ];
  const dat = object(receipt.dat_profile, profileKeys),
    opt = object(receipt.opt_profile, profileKeys);
  check(
    dat.id === "dat-concordance-v1" &&
      dat.field === 20 &&
      dat.qualifier === 254 &&
      dat.newline_in_field === 174 &&
      dat.header_row === true &&
      pageDigest(dat.columns) ===
        pageDigest(["DOCID", "LABELSET", "BEGBATES", "ENDBATES", "PDF", "TEXT", "PAGECOUNT"]),
  );
  check(
    opt.id === "opt-standard-v1" &&
      opt.field === 44 &&
      opt.qualifier === 0 &&
      opt.newline_in_field === 0 &&
      opt.header_row === false &&
      pageDigest(opt.columns) ===
        pageDigest([
          "ImageKey",
          "VolumeName",
          "ImagePath",
          "DocumentBreak",
          "FolderBreak",
          "BoxBreak",
          "PageCount",
        ]),
  );
  for (const profile of [dat, opt])
    check(
      profile.encoding === "utf-8" &&
        profile.date_format === "" &&
        profile.declared_timezone === "",
    );
  return o as unknown as RecipientPackageDownload;
}

export async function prepareRecipientPackage(
  session: string,
  inputs: NumberingInputs,
  artifact: BatesExport,
  signal: AbortSignal,
) {
  recipientText(inputs);
  const value = await buildRecipientPackage(
    {
      allocation_id: artifact.allocation_id,
      preparation: inputs.preparation,
      masks: inputs.masks,
      review_sha256: inputs.review.sha256,
    },
    { session, signal, responseLimit: metadataLimit },
  );
  return decodeRecipientPackage(value, inputs, artifact);
}

export function releaseRecipientPackage(session: string, signal?: AbortSignal, keepalive = false) {
  return discardRecipientPackages({ session, signal, keepalive });
}
