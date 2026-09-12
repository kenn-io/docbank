import bridgePath from "./email-frame-bridge.js?url&no-inline";
import stylePath from "./email-frame.css?url&no-inline";

function attribute(value: string): string { return value.replaceAll("&","&amp;").replaceAll('"',"&quot;").replaceAll("<","&lt;").replaceAll(">","&gt;"); }
export function emailFrameDocument(html: string, nonce: string, origin: string): string {
  const parsed = new URL(origin);
  if (!["http:","https:"].includes(parsed.protocol) || parsed.origin !== origin || !/^[a-f0-9-]{36}$/.test(nonce)) throw new Error("Invalid email frame identity.");
  const script = new URL(bridgePath,origin).href; const style = new URL(stylePath,origin).href;
  const csp = `default-src 'none'; connect-src 'none'; img-src data:; script-src ${script}; style-src ${style}; style-src-attr 'none'; font-src 'none'; media-src 'none'; object-src 'none'; frame-src 'none'; base-uri 'none'; form-action 'none'`;
  return `<!doctype html><html data-email-nonce="${nonce}" data-email-origin="${attribute(origin)}"><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${attribute(csp)}"><meta name="referrer" content="no-referrer"><link rel="stylesheet" href="${attribute(style)}"></head><body>${html}<script src="${attribute(script)}"></script></body></html>`;
}
