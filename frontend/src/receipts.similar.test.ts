import { describe, expect, it, vi, afterEach } from "vitest";
import contract from "../../internal/daemonconn/testdata/processing_responses.json";
import { documentSimilar, validateDocumentSimilarReport } from "./receipts.js";
import type { DocumentSimilarRequest } from "./generated/docbank.js";

afterEach(() => vi.restoreAllMocks());

describe("similar receipts", () => {
  const request = contract.similar_request as DocumentSimilarRequest;
  it.each(contract.similar_cases)("$name", ({patch, valid}) => {
    const validate = () => validateDocumentSimilarReport({...contract.similar_report, ...patch}, request);
    if (valid) expect(validate).not.toThrow();
    else expect(validate).toThrow(/invalid .*response/);
  });
  it("uses the generated similar route", async () => {
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json(contract.similar_report));
    await documentSimilar("session", request);
    expect(String(fetch.mock.calls[0]?.[0])).toBe("/api/v1/search/similar");
  });
  it("rejects limit=101 and nonfinite scores", () => {
    expect(() => validateDocumentSimilarReport(contract.similar_report, {...request,limit:101})).toThrow();
    for (const score of [Infinity, NaN]) {
      expect(() => validateDocumentSimilarReport({...contract.similar_report,results:[{...contract.similar_report.results[0],score}]},request)).toThrow();
    }
  });
});
