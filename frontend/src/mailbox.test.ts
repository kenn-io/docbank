import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vitest";
import * as api from "./api-transport.js";
import { uploadMailboxArchive, type MailboxContainer } from "./mailbox.js";

const hash = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad";
const chunk = { index: 0, sha256: hash, size: 3 };
const container: MailboxContainer = {
  id: "source", sha256: hash, size: 3, format: "mbox", state: "uploading",
  chunks: [chunk], manifest_sha256: "",
};
const sealed = {
  ...container, state: "sealed",
  manifest_sha256: createHash("sha256").update(JSON.stringify({ sha256: hash, size: 3, chunks: [chunk] })).digest("hex"),
};
afterEach(() => vi.restoreAllMocks());

it.each(["uploading", "sealed"])("does not resend verified chunks from a %s source", async (state) => {
  vi.spyOn(api, "sessionJSON").mockResolvedValueOnce({ ...container, state }).mockResolvedValueOnce(sealed);
  const uploadMailboxChunk = vi.fn();
  const result = await uploadMailboxArchive("session", { uploadMailboxChunk }, new File(["abc"], "mail.mbox"), "source", new AbortController().signal, () => {});
  expect(result).toEqual(sealed);
  expect(uploadMailboxChunk).not.toHaveBeenCalled();
});

it("uploads a chunk that has not been saved", async () => {
  vi.spyOn(api, "sessionJSON").mockResolvedValueOnce({ ...container, chunks: [] }).mockResolvedValueOnce(sealed);
  const uploadMailboxChunk = vi.fn();
  await uploadMailboxArchive("session", { uploadMailboxChunk }, new File(["abc"], "mail.mbox"), "source", new AbortController().signal, () => {});
  expect(uploadMailboxChunk).toHaveBeenCalledWith("source", 0, expect.any(Blob), hash, expect.any(AbortSignal), expect.any(Function));
});

it.each([{ ...chunk, sha256: "a".repeat(64) }, { ...chunk, size: 2 }])("rejects a saved chunk that differs from the selected file: %j", async (saved) => {
  vi.spyOn(api, "sessionJSON").mockResolvedValueOnce({ ...container, chunks: [saved] }).mockResolvedValueOnce(sealed);
  const uploadMailboxChunk = vi.fn();
  await expect(uploadMailboxArchive("session", { uploadMailboxChunk }, new File(["abc"], "mail.mbox"), "source", new AbortController().signal, () => {})).rejects.toThrow(/chunk.*match/i);
  expect(uploadMailboxChunk).not.toHaveBeenCalled();
});
