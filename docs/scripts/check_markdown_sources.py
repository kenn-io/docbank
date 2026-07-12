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
DECISIONS = ROOT / "internal" / "decisions"


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


def check_decisions(errors: list[str]) -> None:
    index_path = DECISIONS / "README.md"
    if not index_path.is_file():
        errors.append("internal/decisions/README.md: decision index is missing")
        return

    index = index_path.read_text(encoding="utf-8")
    index_section = re.search(r"(?ms)^## Index\s*$\n(.*?)(?=^## |\Z)", index)
    if index_section is None:
        errors.append("internal/decisions/README.md: missing ## Index section")
        indexed: set[str] = set()
    else:
        indexed = set(re.findall(r"\((\d{4}-[^)]+\.md)\)", index_section.group(1)))

    candidates = sorted(path for path in DECISIONS.glob("*.md") if path.name != "README.md")
    records: list[pathlib.Path] = []
    for path in candidates:
        if re.fullmatch(r"\d{4}-[a-z0-9]+(?:[.-][a-z0-9]+)*\.md", path.name) is None:
            errors.append(f"internal/decisions/{path.name}: invalid decision filename")
        else:
            records.append(path)
    names = {path.name for path in records}
    for missing in sorted(names - indexed):
        errors.append(f"internal/decisions/{missing}: record is missing from index")
    for missing in sorted(indexed - names):
        errors.append(f"internal/decisions/README.md: indexed record {missing} does not exist")

    required = (
        "- **Status:**",
        "- **Date:**",
        "## Context",
        "## Decision",
        "## Consequences",
        "## Alternatives rejected",
        "## Public architecture",
    )
    for path in records:
        text = path.read_text(encoding="utf-8")
        expected_title = f"# ADR-{path.name[:4]}:"
        if not text.startswith(expected_title):
            errors.append(f"internal/decisions/{path.name}: title must start with {expected_title}")
        for marker in required:
            if marker not in text:
                errors.append(f"internal/decisions/{path.name}: missing {marker}")


def main() -> None:
    errors: list[str] = []
    files = public_markdown()
    public = {path.relative_to(ROOT).as_posix() for path in files}
    config = tomllib.loads((ROOT / "zensical.toml").read_text(encoding="utf-8"))
    configured = nav_paths(config["project"]["nav"])
    check_decisions(errors)
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
