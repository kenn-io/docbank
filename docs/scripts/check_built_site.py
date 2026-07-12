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


def main() -> None:
    site = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else "site").resolve()
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

    if errors:
        print("built documentation validation failed:", file=sys.stderr)
        for error in errors:
            print(f"  {error}", file=sys.stderr)
        raise SystemExit(1)
    print(f"built documentation validation passed ({len(pages)} HTML pages)")


if __name__ == "__main__":
    main()
