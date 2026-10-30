import { formatRawBytes } from "./format.js";

// What a provenance fact's source kind means to the person reading it.
export function provenanceSourceLabel(kind: string): string {
  switch (kind) {
    case "url":
      return "Added from URL";
    case "cli":
      return "Added from command line";
    case "upload":
      return "Uploaded";
    case "web":
      return "Uploaded in browser";
    case "mail":
    case "mailbox":
    case "msgvault":
      return "Imported from mailbox";
    case "watch":
      return "Added from watched folder";
    case "filesystem":
      return "Added from folder";
    case "photo-import":
      return "Imported photos";
    default:
      return "Imported document";
  }
}

// JSON carries response headers as base64 to preserve their observed bytes.
export function urlFetchHeader(b64: string): string {
  return formatRawBytes(Uint8Array.from(atob(b64).replace(/\\/g, "\\\\"), (c) => c.charCodeAt(0)));
}
