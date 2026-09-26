import { afterEach, expect, it, vi } from "vitest";
import { loadProductionJobStatus } from "./jobStatus.js";

const setID = "11111111-1111-4111-8111-111111111111";
const jobID = "22222222-2222-4222-8222-222222222222";
const queued = { job_id: jobID, set_id: setID, revision: 2, state: "queued",
  revision_sha256: "a".repeat(64), receipt_sha256: "" };

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function respond(value: object) {
  return vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json(value));
}

it("loads only a set-scoped job with a valid retained state", async () => {
  const fetch = respond(queued);
  expect(await loadProductionJobStatus("synthetic", setID, jobID,
    new AbortController().signal)).toEqual(queued);
  expect(fetch.mock.calls[0]?.[0]).toBe(`/api/v1/productions/sets/${setID}/jobs/${jobID}`);
});

it("rejects a job from another set or a mismatched ID", async () => {
  respond({ ...queued, set_id: "33333333-3333-4333-8333-333333333333" });
  await expect(loadProductionJobStatus("synthetic", setID, jobID,
    new AbortController().signal)).rejects.toThrow(/set|job/i);
  vi.restoreAllMocks();
  respond({ ...queued, job_id: "33333333-3333-4333-8333-333333333333" });
  await expect(loadProductionJobStatus("synthetic", setID, jobID,
    new AbortController().signal)).rejects.toThrow(/set|job/i);
});

it("requires a receipt only for a successful job", async () => {
  respond({ ...queued, state: "succeeded", receipt_sha256: "b".repeat(64) });
  expect((await loadProductionJobStatus("synthetic", setID, jobID,
    new AbortController().signal)).state).toBe("succeeded");
  vi.restoreAllMocks();
  respond({ ...queued, state: "succeeded" });
  await expect(loadProductionJobStatus("synthetic", setID, jobID,
    new AbortController().signal)).rejects.toThrow(/receipt|status/i);
  vi.restoreAllMocks();
  respond({ ...queued, state: "running", receipt_sha256: "b".repeat(64) });
  await expect(loadProductionJobStatus("synthetic", setID, jobID,
    new AbortController().signal)).rejects.toThrow(/receipt|status/i);
});

it("rejects malformed IDs before making a request", async () => {
  const fetch = vi.spyOn(globalThis, "fetch");
  await expect(loadProductionJobStatus("synthetic", setID, "not-a-job",
    new AbortController().signal)).rejects.toThrow(/job id/i);
  expect(fetch).not.toHaveBeenCalled();
});
