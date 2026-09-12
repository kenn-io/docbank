import { describe, expect, it } from "vitest";
import { validatePDFReceipt } from "./email-pdf.js";

describe("email PDF exact selection", () => {
  it("rejects a receipt for another original or recipe", () => {
    expect(() => validatePDFReceipt({ source: { version_id: "wrong" } }, "selected", "a".repeat(64))).toThrow("selected");
  });
});
