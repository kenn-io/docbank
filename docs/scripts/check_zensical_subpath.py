#!/usr/bin/env python3
from __future__ import annotations

import html.parser
import pathlib
import shutil
import subprocess
import tempfile
import urllib.parse


DOCS_ROOT = pathlib.Path(__file__).resolve().parents[1]


class ParsedPage(html.parser.HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.canonical = ""
        self.metadata: dict[str, str] = {}
        self.urls: list[str] = []

    def handle_starttag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        values = {key: value or "" for key, value in attrs}
        if tag == "link" and values.get("rel") == "canonical":
            self.canonical = values.get("href", "")
        if tag == "meta" and values.get("property") and values.get("content"):
            self.metadata[values["property"]] = values["content"]
        for key in ("href", "src"):
            if values.get(key):
                self.urls.append(values[key])

    @classmethod
    def from_file(cls, path: pathlib.Path) -> ParsedPage:
        parsed = cls()
        parsed.feed(path.read_text(encoding="utf-8"))
        return parsed


def write_project(root: pathlib.Path) -> pathlib.Path:
    source = root / "source"
    output = root / "site"
    (source / "usage").mkdir(parents=True)
    (source / "stylesheets").mkdir()
    shutil.copytree(DOCS_ROOT / "overrides", root / "overrides")
    (source / "index.md").write_text(
        "---\ntitle: Overview\ndescription: Subpath overview.\n---\n\n"
        "# Overview\n\n[Example](usage/example.md)\n",
        encoding="utf-8",
    )
    (source / "usage" / "example.md").write_text(
        "---\ntitle: Example\ndescription: Subpath example.\n---\n\n"
        "# Example\n\n[Overview](../index.md)\n",
        encoding="utf-8",
    )
    shutil.copyfile(
        DOCS_ROOT / "stylesheets" / "extra.css",
        source / "stylesheets" / "extra.css",
    )

    config = root / "zensical.toml"
    config.write_text(
        "\n".join(
            [
                "[project]",
                'site_name = "docbank subpath probe"',
                'site_url = "https://docbank.ai/docs/"',
                'site_description = "Executable Zensical subpath probe."',
                'docs_dir = "source"',
                'site_dir = "site"',
                'extra_css = ["stylesheets/extra.css"]',
                "use_directory_urls = true",
                'nav = [{"Overview" = "index.md"}, {"Usage" = [{"Example" = "usage/example.md"}]}]',
                "",
                "[project.theme]",
                'variant = "modern"',
                "font = false",
                'custom_dir = "overrides"',
                "",
            ]
        ),
        encoding="utf-8",
    )
    return output


def assert_site(output: pathlib.Path) -> None:
    expected = {
        "index.html": "https://docbank.ai/docs/",
        "usage/example/index.html": "https://docbank.ai/docs/usage/example/",
    }
    for relative, canonical in expected.items():
        page = output / relative
        if not page.is_file():
            raise AssertionError(f"missing rendered page: {relative}")
        parsed = ParsedPage.from_file(page)
        if parsed.canonical != canonical:
            raise AssertionError(
                f"{relative}: canonical is {parsed.canonical!r}, expected {canonical!r}"
            )
        if parsed.metadata.get("og:url") != canonical:
            raise AssertionError(
                f"{relative}: og:url is {parsed.metadata.get('og:url')!r}, "
                f"expected {canonical!r}"
            )
        for raw_url in parsed.urls:
            resolved = urllib.parse.urljoin(canonical, raw_url)
            parsed_url = urllib.parse.urlsplit(resolved)
            if parsed_url.netloc == "docbank.ai" and not parsed_url.path.startswith(
                "/docs/"
            ):
                raise AssertionError(
                    f"{relative}: local URL escapes /docs/: {raw_url!r}"
                )

def main() -> None:
    with tempfile.TemporaryDirectory(
        prefix="zensical-subpath-", dir=DOCS_ROOT
    ) as temporary:
        root = pathlib.Path(temporary)
        output = write_project(root)
        subprocess.run(
            [
                "uv",
                "run",
                "--project",
                str(DOCS_ROOT),
                "--frozen",
                "--no-dev",
                "zensical",
                "build",
                "--strict",
                "--config-file",
                str(root / "zensical.toml"),
            ],
            cwd=DOCS_ROOT,
            check=True,
        )
        assert_site(output)
    print("Zensical /docs/ subpath behavior passed")


if __name__ == "__main__":
    main()
