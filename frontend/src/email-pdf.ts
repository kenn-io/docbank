import { requestJSON } from "./api.js";

export interface EmailPDFReceipt {
  source: { node_id: number; version_id: string; sha256: string; size: number };
  attachment_id: string;
  profile_fingerprint: string;
  binding: { generation_id: string; recipe: { paper: string } };
  output: { pdf_sha256: string; pdf_size: number; pages: number };
}

export function validatePDFReceipt(value: unknown, versionID: string, profile?: string): EmailPDFReceipt {
  const r = value as Partial<EmailPDFReceipt> | null;
  const hash = (v: unknown) => typeof v === "string" && /^[a-f0-9]{64}$/.test(v);
  if (!r || r.source?.version_id !== versionID || !hash(r.source.sha256) || !hash(r.attachment_id) || !hash(r.profile_fingerprint) || (profile && r.profile_fingerprint !== profile) || !hash(r.binding?.generation_id) || !["A4", "Letter"].includes(r.binding?.recipe?.paper ?? "") || !hash(r.output?.pdf_sha256) || !Number.isSafeInteger(r.output?.pdf_size) || (r.output?.pdf_size ?? 0) < 1 || (r.output?.pdf_size ?? Infinity) > 256 * 1024 * 1024 || !Number.isSafeInteger(r.output?.pages) || (r.output?.pages ?? 0) < 1 || (r.output?.pages ?? Infinity) > 1000) {
    throw new Error("The PDF receipt disagrees with the selected email version or recipe.");
  }
  return r as EmailPDFReceipt;
}

export async function retainedEmailPDFs(session: string, versionID: string, signal: AbortSignal): Promise<EmailPDFReceipt[]> {
  const values = await requestJSON<unknown[]>(`/api/v1/email-pdfs/${encodeURIComponent(versionID)}`, session, { signal });
  if (!Array.isArray(values)) throw new Error("Invalid retained PDF response.");
  return values.map((v) => validatePDFReceipt(v, versionID));
}

export async function renderEmailPDF(session: string, versionID: string, paper: string, signal: AbortSignal): Promise<EmailPDFReceipt> {
  const job = await requestJSON<{ job_id: string; version_id: string; profile_fingerprint: string; state: string }>("/api/v1/email-pdfs", session, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ version_id: versionID, paper, consent: true }), signal });
  if (job.version_id !== versionID || !/^[a-f0-9]{64}$/.test(job.profile_fingerprint) || !/^[a-f0-9]{64}$/.test(job.job_id)) throw new Error("The PDF job disagrees with the selected version.");
  while (job.state !== "completed") {
    if (!["queued", "running", "retry_wait"].includes(job.state)) throw new Error(`PDF rendering ended ${job.state}; inspect processing jobs.`);
    await new Promise<void>((resolve, reject) => {
      if (signal.aborted) { reject(signal.reason); return; }
      const abort = () => { clearTimeout(timer); reject(signal.reason); };
      const timer = setTimeout(() => { signal.removeEventListener("abort", abort); resolve(); }, 500);
      signal.addEventListener("abort", abort, { once: true });
    });
    const next = await requestJSON<{ state: string }>(`/api/v1/email-pdf-jobs/${encodeURIComponent(job.job_id)}`, session, { signal });
    job.state = next.state;
  }
  const receipt = await requestJSON<unknown>(`/api/v1/email-pdfs/${encodeURIComponent(versionID)}/${job.profile_fingerprint}`, session, { signal });
  return validatePDFReceipt(receipt, versionID, job.profile_fingerprint);
}
