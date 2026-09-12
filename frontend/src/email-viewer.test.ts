import { describe, expect, it, vi } from "vitest";
import { sanitizeEmail, resolveCID, rasterSize, validFrameEscape, readBounded, readEmailPart, type EmailMetadata, type EmailPart } from "./email-viewer.js";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";

describe("offline email presentation", () => {
  it("removes executable and network-bearing surfaces while retaining full quoted text", () => {
    const quote = "Quoted evidence. ".repeat(40);
    const result = sanitizeEmail(`<script>alert(1)</script><style>@import 'https://tracker.test/x';</style><form><input name=x></form><p onclick=x style="background:url(https://tracker.test/i)">Hello</p><img src="https://tracker.test/a"><img src="cid:logo"><blockquote>${quote}</blockquote><svg><image href="https://tracker.test/b"/></svg>`);
    const template = document.createElement("template"); template.innerHTML = result.html;
    expect(template.content.querySelector("script,style,form,input,svg,[onclick],[style],[src],[href]")).toBeNull();
    expect(template.content.textContent).toContain(quote);
    expect(template.content.querySelector("details")?.hasAttribute("open")).toBe(false);
    expect(result.images).toEqual([{ index: 0, cid: "logo", alt: "Inline image" }]);
    expect(result.warnings.length).toBeGreaterThan(0);
  });
  it("never guesses CID ownership across nearest related groups or nested messages", () => {
    const parts = [
      { path: "1", parent_path: null, message_path: "1", media: { declared: "multipart/related" }, content_id: { value: null } },
      { path: "1.1", parent_path: "1", message_path: "1", media: { declared: "multipart/related" }, content_id: { value: null } },
      { path: "1.1.1", parent_path: "1.1", message_path: "1", media: { declared: "text/html" }, content_id: { value: null } },
      { path: "1.2", parent_path: "1", message_path: "1", media: { declared: "image/png" }, content_id: { value: "logo", state: "decoded" } },
    ];
    expect(resolveCID(parts as never, "1.1.1", "logo")).toBeUndefined();
    parts.push({ path: "1.1.2", parent_path: "1.1", message_path: "1", media: { declared: "image/png" }, content_id: { value: "logo", state: "decoded" } } as never);
    expect(resolveCID(parts as never, "1.1.1", "logo")?.path).toBe("1.1.2");
    parts.push({ ...parts[4]!, path: "1.1.3" });
    expect(resolveCID(parts as never, "1.1.1", "logo")).toBeUndefined();
  });
  it("rejects oversized raster headers before browser decoding", () => {
    const png = new Uint8Array(24); png.set([137,80,78,71,13,10,26,10]); png.set([73,72,68,82],12);
    const view = new DataView(png.buffer); view.setUint32(16,100000); view.setUint32(20,100000);
    expect(() => rasterSize(png)).toThrow(/pixel|raster/i);
    view.setUint32(16,32); view.setUint32(20,16);
    expect(rasterSize(png)).toEqual({ width:32, height:16, mime:"image/png" });
  });
  it("cancels over-limit streams and rejects stale reads", async () => {
    const cancel = vi.fn();
    const body = new ReadableStream({ start(c) { c.enqueue(new Uint8Array(8)); }, cancel });
    await expect(readBounded(new Response(body), 4, new AbortController().signal)).rejects.toThrow(/limit/);
    expect(cancel).toHaveBeenCalled();
    const controller = new AbortController(); controller.abort();
    await expect(readBounded(new Response("old"), 10, controller.signal)).rejects.toThrow();
  });
  it("accepts only the exact opaque frame and current nonce escape message", () => {
    const frame = {} as Window;
    const message = { channel: "docbank-email", nonce: "fresh", action: "escape" };
    expect(validFrameEscape({ source: frame, origin: "null", data: message } as MessageEvent, frame, "fresh")).toBe(true);
    for (const event of [{ source: {}, origin:"null", data:message }, { source:frame, origin:"https://sender.test", data:message }, { source:frame, origin:"null", data:{ ...message, nonce:"old" } }, { source:frame, origin:"null", data:{ ...message, extra:1 } }]) {
      expect(validFrameEscape(event as MessageEvent, frame, "fresh")).toBe(false);
    }
  });
  it("rejects wrong version, generation, occurrence, role, hash, size and corrupt part bytes", async () => {
    const bytes = new TextEncoder().encode("verified body");
    const artifact = { role:"body_utf8" as const,sha256:bytesToHex(sha256(bytes)),size:bytes.length };
    const part = { path:"1.2",body_utf8:artifact } as EmailPart;
    const metadata = { version:{ id:"source-version" },generation_id:"a".repeat(64),attachment_id:"b".repeat(64),evidence:{ inventory:{ parts:[part] } } } as EmailMetadata;
    const headers = { "Content-Type":"application/octet-stream", "X-Docbank-Content-Version":"source-version", "X-Docbank-Email-Generation":"a".repeat(64), "X-Docbank-Email-Attachment":"b".repeat(64), "X-Docbank-Email-Part-Path":"1.2", "X-Docbank-Email-Part-Role":"body_utf8", "X-Docbank-Blob-Hash":artifact.sha256, "X-Docbank-Blob-Size":String(bytes.length) };
    try {
      vi.stubGlobal("fetch",vi.fn(async () => new Response(bytes,{ headers })));
      expect(Array.from(await readEmailPart("session",metadata,part,artifact,new AbortController().signal))).toEqual(Array.from(bytes));
      for (const key of Object.keys(headers)) {
        vi.stubGlobal("fetch",vi.fn(async () => new Response(bytes,{ headers:{ ...headers,[key]:"wrong" } })));
        await expect(readEmailPart("session",metadata,part,artifact,new AbortController().signal)).rejects.toThrow(/identity/);
      }
      vi.stubGlobal("fetch",vi.fn(async () => new Response(new Uint8Array(bytes.length),{ headers })));
      await expect(readEmailPart("session",metadata,part,artifact,new AbortController().signal)).rejects.toThrow(/SHA-256/);
      vi.stubGlobal("fetch",vi.fn(async () => new Response(bytes.subarray(1),{ headers })));
      await expect(readEmailPart("session",metadata,part,artifact,new AbortController().signal)).rejects.toThrow(/size/);
    } finally { vi.unstubAllGlobals(); }
  });
  it("accepts the full 16 MiB HTML operation and refuses excess without truncation", () => {
    const result = sanitizeEmail("x".repeat(16 * 1024 * 1024));
    expect(result.html.length).toBe(16 * 1024 * 1024);
    expect(() => sanitizeEmail("x".repeat(16 * 1024 * 1024+1))).toThrow(/16 MiB/);
    expect(() => sanitizeEmail("<i>".repeat(100_001))).toThrow(/before parsing/);
  });
});
