#!/usr/bin/env python3
from __future__ import annotations

import html.parser
import pathlib
import sys
import urllib.parse


class PageParser(html.parser.HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.title = False
        self.description = False
        self.urls: list[str] = []
        self.anchors: set[str] = set()

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        values = {key: value or "" for key, value in attrs}
        if tag == "title":
            self.title = True
        if tag == "meta" and values.get("name") == "description" and values.get("content"):
            self.description = True
        if values.get("id"):
            self.anchors.add(values["id"])
        if tag == "a" and values.get("name"):
            self.anchors.add(values["name"])
        for key in ("href", "src"):
            if key in values:
                self.urls.append(values[key])


def local_target(
    site: pathlib.Path, page: pathlib.Path, raw: str
) -> tuple[pathlib.Path, str] | None:
    parsed = urllib.parse.urlsplit(raw)
    if parsed.scheme or parsed.netloc:
        return None
    if parsed.path:
        target = site / parsed.path.lstrip("/") if parsed.path.startswith("/") else page.parent / parsed.path
        if parsed.path.endswith("/") or target.suffix == "":
            target /= "index.html"
    else:
        target = page
    return target.resolve(), urllib.parse.unquote(parsed.fragment)


def markdown_route(site: pathlib.Path, rel: pathlib.Path) -> pathlib.Path:
    if rel.name == "index.md":
        return site / rel.parent / "index.html"
    return site / rel.with_suffix("") / "index.html"


def markdown_output(site: pathlib.Path, rel: pathlib.Path) -> pathlib.Path:
    if rel.name == "index.md" and rel.parent != pathlib.Path("."):
        return site / rel.parent.with_suffix(".md")
    return site / rel


def main() -> None:
    site = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "site").resolve()
    source = pathlib.Path(sys.argv[2]).resolve() if len(sys.argv) > 2 else None
    errors: list[str] = []
    pages = sorted(site.rglob("*.html"))
    if not pages:
        errors.append(f"no HTML pages built under {site}")
    forbidden = {"internal", "superpowers", "scripts", "zensical.toml", "pyproject.toml", "uv.lock"}
    for path in site.rglob("*"):
        if forbidden.intersection(path.relative_to(site).parts):
            errors.append(f"publishing boundary leaked {path.relative_to(site)}")

    parsed_pages: dict[pathlib.Path, PageParser] = {}
    for page in pages:
        parser = PageParser()
        parser.feed(page.read_text(encoding="utf-8"))
        parsed_pages[page.resolve()] = parser

    for page in pages:
        parser = parsed_pages[page.resolve()]
        rel = page.relative_to(site)
        if not parser.title:
            errors.append(f"{rel}: missing title")
        if not parser.description:
            errors.append(f"{rel}: missing meta description")
        for raw in parser.urls:
            local = local_target(site, page, raw)
            if local is None:
                continue
            target, fragment = local
            try:
                target.relative_to(site)
            except ValueError:
                errors.append(f"{rel}: local URL escapes site: {raw}")
                continue
            if not target.exists():
                errors.append(f"{rel}: broken local URL {raw}")
            elif (
                fragment
                and fragment != "__skip"
                and target in parsed_pages
                and fragment not in parsed_pages[target].anchors
            ):
                errors.append(f"{rel}: broken local fragment {raw}")

    if source is not None:
        source_markdown = sorted(source.rglob("*.md"))
        expected_markdown: set[pathlib.Path] = set()
        for markdown in source_markdown:
            rel = markdown.relative_to(source)
            published = markdown_output(site, rel)
            expected_markdown.add(published.resolve())
            if not published.is_file():
                errors.append(f"{rel}: Markdown counterpart was not published")
            elif published.read_bytes() != markdown.read_bytes():
                errors.append(f"{rel}: published Markdown differs from its source")

            route = markdown_route(site, rel)
            if not route.is_file():
                errors.append(f"{rel}: rendered route does not exist")

        for published in site.rglob("*.md"):
            if published.resolve() not in expected_markdown:
                errors.append(f"{published.relative_to(site)}: no matching Markdown source")

        for page in pages:
            rel = page.relative_to(site)
            if rel == pathlib.Path("404.html"):
                continue
            markdown = (
                site / "index.md"
                if rel == pathlib.Path("index.html")
                else page.parent.with_suffix(".md")
            )
            if not markdown.is_file():
                errors.append(f"{rel}: rendered route has no Markdown counterpart")

    if errors:
        print("built documentation validation failed:", file=sys.stderr)
        for error in errors:
            print(f"  {error}", file=sys.stderr)
        raise SystemExit(1)
    print(f"built documentation validation passed ({len(pages)} HTML pages)")


if __name__ == "__main__":
    main()
