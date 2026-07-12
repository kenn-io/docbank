#!/usr/bin/env python3
from __future__ import annotations

import pathlib
import re
import sys
import tomllib

ROOT = pathlib.Path(__file__).resolve().parents[1]
EXCLUDED = {".cache", ".venv", "internal", "scripts", "site", "superpowers"}
FORBIDDEN = ("@astrojs/starlight", "<Card", "<Aside", "<Tabs", ":::")
ADMONITION = re.compile(r'!!!\s+[A-Za-z][\w-]*(?:\s+"(?:[^"\\]|\\.)*")?')


def public_markdown() -> list[pathlib.Path]:
    return sorted(
        path
        for path in ROOT.rglob("*.md")
        if path.name != "README.md"
        and not any(
            part in EXCLUDED or part.startswith("zensical-public-docs.")
            for part in path.relative_to(ROOT).parts
        )
    )


def nav_paths(value: object) -> set[str]:
    result: set[str] = set()
    if isinstance(value, str):
        result.add(value)
    elif isinstance(value, list):
        for item in value:
            result.update(nav_paths(item))
    elif isinstance(value, dict):
        for item in value.values():
            result.update(nav_paths(item))
    return result


def main() -> None:
    errors: list[str] = []
    files = public_markdown()
    public = {path.relative_to(ROOT).as_posix() for path in files}
    config = tomllib.loads((ROOT / "zensical.toml").read_text(encoding="utf-8"))
    configured = nav_paths(config["project"]["nav"])
    for missing in sorted(public - configured):
        errors.append(f"{missing}: public page is missing from navigation")
    for missing in sorted(configured - public):
        errors.append(f"{missing}: navigation target does not exist")

    for path in files:
        rel = path.relative_to(ROOT)
        if rel.name == "index.md" and rel != pathlib.Path("index.md"):
            section_page = pathlib.Path(f"{rel.parent}.md")
            errors.append(
                f"{rel}: nested index pages are unsupported; use {section_page} "
                "so the rendered and Markdown routes share a URL base"
            )
        text = path.read_text(encoding="utf-8")
        lines = text.splitlines()
        if len(lines) < 4 or lines[0] != "---":
            errors.append(f"{rel}: missing YAML frontmatter")
        else:
            try:
                closing = lines[1:80].index("---") + 1
            except ValueError:
                errors.append(f"{rel}: missing closing frontmatter delimiter")
            else:
                frontmatter = "\n".join(lines[1:closing])
                for field in ("title", "description"):
                    if re.search(rf"(?m)^{field}:\s+\S", frontmatter) is None:
                        errors.append(f"{rel}: missing {field} in frontmatter")
        for number, line in enumerate(lines, start=1):
            stripped = line.strip()
            if stripped.startswith("!!! "):
                if ADMONITION.fullmatch(stripped) is None:
                    errors.append(f"{rel}:{number}: malformed admonition")
                following = next((item for item in lines[number:] if item.strip()), "")
                if not following.startswith("    "):
                    errors.append(f"{rel}:{number}: admonition body must be indented")
        if re.search(r"(?m)^import\s+(?:[A-Za-z_$][\w$]*|\{[^}]+\}|\*\s+as\s+)\s+from\s+['\"]", text):
            errors.append(f"{rel}: unsupported MDX import")
        for marker in FORBIDDEN:
            if marker in text:
                errors.append(f"{rel}: unsupported markup {marker!r}")

    if errors:
        print("documentation source validation failed:", file=sys.stderr)
        for error in errors:
            print(f"  {error}", file=sys.stderr)
        raise SystemExit(1)
    print(f"documentation source validation passed ({len(files)} public pages)")


if __name__ == "__main__":
    main()
