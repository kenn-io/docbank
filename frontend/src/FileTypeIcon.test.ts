import { describe, expect, it } from "vitest";
import { fileFamily, fileTypeLabel } from "./FileTypeIcon.svelte";

describe("file type presentation", () => {
  it("names common document types and keeps unknown MIME types visible", () => {
    expect(fileTypeLabel("dir", undefined)).toBe("Folder");
    expect(fileTypeLabel("file", "text/plain; charset=utf-8")).toBe("Plain text");
    expect(fileTypeLabel("file", "application/pdf")).toBe("PDF");
    expect(fileTypeLabel("file", "image/x-png")).toBe("PNG image");
    expect(fileTypeLabel("file", "application/x-custom")).toBe("application/x-custom");
    expect(fileTypeLabel("file", "")).toBe("File");
  });

  it("groups MIME types into icon families", () => {
    expect(fileFamily("dir", "text/plain")).toBe("folder");
    expect(fileFamily("file", "text/csv")).toBe("sheet");
    expect(fileFamily("file", "message/rfc822")).toBe("email");
    expect(fileFamily("file", "application/octet-stream")).toBe("file");
  });
});
