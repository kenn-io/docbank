// API paths and application routes stay relative to Docbank's own root.
// The server supplies the mount explicitly; never infer it from a deep link.
export const webBasePath = (): string =>
  globalThis.document?.querySelector<HTMLMetaElement>('meta[name="docbank-base-path"]')?.content ??
  "";

export function webURL(path: string): string {
  return path.startsWith("/") && !path.startsWith("//") ? webBasePath() + path : path;
}

export function webPath(pathname = location.pathname): string {
  const base = webBasePath();
  return pathname.startsWith(base + "/") ? pathname.slice(base.length) : "";
}
