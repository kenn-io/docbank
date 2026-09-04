import { lstat, readFile, readdir } from "node:fs/promises";
import path from "node:path";


const canonicalOrigin = "https://docbank.ai";
const forbiddenParts = new Set([
  ".git",
  ".superpowers",
  "internal",
  "overrides",
  "reports",
  "superpowers",
]);
const requiredMetadata = [
  "description",
  "og:type",
  "og:title",
  "og:description",
  "og:url",
  "og:site_name",
  "twitter:card",
  "twitter:title",
  "twitter:description",
];


async function filesUnder(root) {
  const files = [];
  async function visit(directory, prefix = "") {
    for (const entry of await readdir(directory, { withFileTypes: true })) {
      const relative = prefix ? `${prefix}/${entry.name}` : entry.name;
      const absolute = path.join(directory, entry.name);
      const metadata = await lstat(absolute);
      if (metadata.isSymbolicLink()) {
        throw new Error(`publishing boundary contains symlink: ${relative}`);
      }
      if (metadata.isDirectory()) await visit(absolute, relative);
      else if (metadata.isFile()) files.push(relative);
    }
  }
  await visit(root);
  return files.sort();
}


function attributes(raw) {
  const result = new Map();
  const expression = /([A-Za-z_:][A-Za-z0-9_.:-]*)\s*=\s*(?:"([^"]*)"|'([^']*)')/g;
  for (const match of raw.matchAll(expression)) {
    result.set(match[1].toLowerCase(), match[2] ?? match[3] ?? "");
  }
  return result;
}


function parsePage(contents) {
  const metadata = new Map();
  const urls = [];
  const images = [];
  const anchors = new Set();
  let canonical = "";
  for (const match of contents.matchAll(/<([A-Za-z0-9]+)\b([^>]*)>/g)) {
    const tag = match[1].toLowerCase();
    const attrs = attributes(match[2]);
    if (attrs.get("id")) anchors.add(attrs.get("id"));
    if (tag === "a" && attrs.get("name")) anchors.add(attrs.get("name"));
    if (tag === "link" && attrs.get("rel") === "canonical") {
      canonical = attrs.get("href") ?? "";
    }
    if (tag === "meta" && attrs.get("content")) {
      const key = attrs.get("property") ?? attrs.get("name");
      if (key) metadata.set(key, attrs.get("content"));
    }
    for (const key of ["href", "src"]) {
      if (attrs.get(key)) urls.push(attrs.get(key));
    }
    if (tag === "img" && attrs.get("src")) images.push(attrs.get("src"));
  }
  return {
    anchors,
    canonical,
    images,
    metadata,
    title: /<title(?:\s[^>]*)?>\s*[^<\s][^<]*<\/title>/i.test(contents),
    urls,
  };
}


function routeForHtml(relative) {
  if (relative === "index.html") return "/";
  return `/${relative.replace(/index\.html$/, "")}`;
}


function markdownPeer(relative) {
  if (relative === "index.html") return "index.md";
  if (relative === "guide/index.html") return "guide.md";
  if (relative === "docs/index.html") return "docs/index.md";
  if (!relative.endsWith("/index.html")) return null;
  return relative.replace(/\/index\.html$/, ".md");
}


function htmlForMarkdown(relative) {
  if (relative === "index.md") return "index.html";
  if (relative === "guide.md") return "guide/index.html";
  if (relative === "docs/index.md") return "docs/index.html";
  return relative.replace(/\.md$/, "/index.html");
}


function targetForUrl(site, page, raw) {
  if (/^(?:mailto|tel|data|javascript):/i.test(raw)) return null;
  const base = `${canonicalOrigin}${routeForHtml(page)}`;
  let resolved;
  try {
    resolved = new URL(raw, base);
  } catch {
    return { error: "invalid" };
  }
  if (resolved.origin !== canonicalOrigin) return null;
  let relative = decodeURIComponent(resolved.pathname).replace(/^\//, "");
  if (relative === "" || relative.endsWith("/")) relative += "index.html";
  else if (path.posix.extname(relative) === "") relative += "/index.html";
  return {
    absolute: path.join(site, ...relative.split("/")),
    fragment: decodeURIComponent(resolved.hash.replace(/^#/, "")),
    relative,
  };
}


async function compareSource(site, relative, source, errors) {
  try {
    const [published, original] = await Promise.all([
      readFile(path.join(site, ...relative.split("/"))),
      readFile(source),
    ]);
    if (!published.equals(original)) {
      errors.push(`${relative}: published Markdown differs from source`);
    }
  } catch (error) {
    if (error?.code === "ENOENT") {
      errors.push(`${relative}: Markdown source is missing`);
      return;
    }
    throw error;
  }
}


export async function verifySite({ site, sources }) {
  const errors = [];
  const files = await filesUnder(site);
  const fileSet = new Set(files);
  for (const relative of files) {
    const parts = relative.split("/");
    if (parts.some((part) => forbiddenParts.has(part))) {
      errors.push(`publishing boundary leaked ${relative}`);
    }
  }

  const htmlFiles = files.filter((relative) => relative.endsWith(".html"));
  if (htmlFiles.length === 0) errors.push("no HTML pages were built");
  const parsedPages = new Map();
  for (const relative of htmlFiles) {
    const contents = await readFile(path.join(site, ...relative.split("/")), "utf8");
    const parsed = parsePage(contents);
    parsedPages.set(relative, parsed);
    if (!parsed.title) errors.push(`${relative}: missing title`);
    for (const key of requiredMetadata) {
      if (!parsed.metadata.get(key)) errors.push(`${relative}: missing ${key} metadata`);
    }
    if (!relative.endsWith("404.html")) {
      const expectedCanonical = `${canonicalOrigin}${routeForHtml(relative)}`;
      if (parsed.canonical !== expectedCanonical) {
        errors.push(`${relative}: canonical is ${parsed.canonical || "missing"}; expected ${expectedCanonical}`);
      }
      if (parsed.metadata.get("og:url") !== expectedCanonical) {
        errors.push(`${relative}: og:url does not match its route`);
      }
    }
    if (/fonts\.(?:googleapis|gstatic)\.com/i.test(contents)) {
      errors.push(`${relative}: remote font URL`);
    }
  }

  for (const relative of htmlFiles) {
    const parsed = parsedPages.get(relative);
    for (const raw of parsed.images) {
      if (targetForUrl(site, relative, raw) === null) {
        errors.push(`${relative}: image is not served from the site: ${raw}`);
      }
    }
    for (const raw of parsed.urls) {
      const target = targetForUrl(site, relative, raw);
      if (target === null) continue;
      if (target.error || !fileSet.has(target.relative)) {
        errors.push(`${relative}: broken local URL ${raw}`);
        continue;
      }
      if (
        target.fragment &&
        target.fragment !== "__skip" &&
        target.relative.endsWith(".html")
      ) {
        const destination = parsedPages.get(target.relative);
        if (destination && !destination.anchors.has(target.fragment)) {
          errors.push(`${relative}: broken local fragment ${raw}`);
        }
      }
    }
  }

  const markdownFiles = files.filter((relative) => relative.endsWith(".md"));
  for (const relative of htmlFiles) {
    if (relative.endsWith("404.html")) continue;
    const peer = markdownPeer(relative);
    if (peer === null) {
      errors.push(`${relative}: unsupported substantive HTML route`);
      continue;
    }
    if (!fileSet.has(peer)) errors.push(`${relative}: missing Markdown peer ${peer}`);
  }
  for (const relative of markdownFiles) {
    const rendered = htmlForMarkdown(relative);
    if (!fileSet.has(rendered)) errors.push(`${relative}: missing HTML route ${rendered}`);
    if (relative === "index.md" || relative === "guide.md") {
      await compareSource(site, relative, path.join(sources.website, relative), errors);
    } else if (relative.startsWith("docs/")) {
      const sourceRelative = relative.slice("docs/".length);
      await compareSource(site, relative, path.join(sources.docs, sourceRelative), errors);
    }
  }

  if (!fileSet.has("llms.txt")) {
    errors.push("llms.txt: missing from site root");
  } else {
    const llms = await readFile(path.join(site, "llms.txt"), "utf8");
    const indexed = new Set(
      [...llms.matchAll(/https:\/\/docbank\.ai(\/[^)\s]+\.md)/g)].map(
        (match) => match[1],
      ),
    );
    for (const relative of markdownFiles) {
      const route = `/${relative}`;
      if (!indexed.has(route)) errors.push(`llms.txt: missing ${route}`);
    }
    for (const route of indexed) {
      if (!fileSet.has(route.slice(1))) errors.push(`llms.txt: missing page ${route}`);
    }
  }

  const generatedPrefix = "assets/generated/";
  const generated = files
    .filter((relative) => relative.startsWith(generatedPrefix))
    .map((relative) => relative.slice(generatedPrefix.length));
  const declared = [...sources.assets].sort();
  for (const name of generated) {
    if (!declared.includes(name)) errors.push(`undeclared generated asset: ${name}`);
  }
  for (const name of declared) {
    if (!generated.includes(name)) errors.push(`missing generated asset: ${name}`);
  }

  for (const relative of files.filter((name) => name.endsWith(".css"))) {
    const contents = await readFile(path.join(site, ...relative.split("/")), "utf8");
    if (/fonts\.(?:googleapis|gstatic)\.com/i.test(contents)) {
      errors.push(`${relative}: remote font URL`);
    }
  }

  if (errors.length > 0) {
    throw new Error(`built site validation failed:\n  ${errors.join("\n  ")}`);
  }
  return { htmlPages: htmlFiles.length, markdownPages: markdownFiles.length };
}
