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
TRACKER_ALLOWLIST = {pathlib.Path("changelog.md"), pathlib.Path("roadmap.md")}
TRACKER_PATTERNS = (
    (re.compile(r"(?im)^\s*(?:[-*]\s+)?\[[ xX]\]\s+"), "task checkbox"),
    (re.compile(r"(?i)\bTODO\b"), "TODO marker"),
    (re.compile(r"(?i)\bPhase\s+[0-9]+[a-z]?\b"), "phase tracking"),
    (re.compile(r"(?i)\bimplementation plans?\b"), "implementation planning"),
    (re.compile(r"(?i)\bremaining work\b"), "remaining-work tracking"),
    (re.compile(r"(?i)\bfollow-on work\b"), "follow-on work tracking"),
    (re.compile(r"(?i)docs/superpowers/"), "transient plan path"),
)


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


def maintained_markdown() -> list[pathlib.Path]:
    return sorted(
        path
        for path in ROOT.rglob("*.md")
        if not any(
            part in {".cache", ".venv", "site"}
            or part.startswith("zensical-public-docs.")
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


LLMS_BASE_URL = "https://docbank.ai/"


def frontmatter_field(path: pathlib.Path, field: str) -> str:
    lines = path.read_text(encoding="utf-8").splitlines()
    for line in lines[1:80]:
        if line == "---":
            break
        match = re.match(rf"{field}:\s+(\S.*)$", line)
        if match is not None:
            return match.group(1).strip()
    return ""


def expected_llms_sections(nav: list) -> list[str]:
    lines: list[str] = []
    for item in nav:
        [(label, value)] = item.items()
        lines.append(f"## {label}")
        targets = [value] if isinstance(value, str) else [
            path for entry in value for path in entry.values()
        ]
        for target in targets:
            page = ROOT / target
            if not page.is_file():
                continue
            title = frontmatter_field(page, "title")
            description = frontmatter_field(page, "description")
            lines.append(f"- [{title}]({LLMS_BASE_URL}{target}): {description}")
        lines.append("")
    return lines


def check_llms_txt(config: dict, errors: list[str]) -> None:
    llms = ROOT / "llms.txt"
    if not llms.is_file():
        errors.append("llms.txt: missing; every published page must be indexed")
        return
    text = llms.read_text(encoding="utf-8")
    lines = text.splitlines()
    site_description = config["project"]["site_description"]
    if f"> {site_description}" not in lines:
        errors.append(
            "llms.txt: blockquote line must match site_description exactly"
        )
    try:
        first_section = next(
            index for index, line in enumerate(lines) if line.startswith("## ")
        )
    except StopIteration:
        errors.append("llms.txt: no '## <section>' headings found")
        return
    actual = [line.rstrip() for line in lines[first_section:]]
    while actual and not actual[-1]:
        actual.pop()
    expected = expected_llms_sections(config["project"]["nav"])
    while expected and not expected[-1]:
        expected.pop()
    if actual != expected:
        for number, (have, want) in enumerate(zip(actual, expected)):
            if have != want:
                errors.append(
                    f"llms.txt: line {first_section + number + 1} is\n"
                    f"    {have!r}\n  expected\n    {want!r}"
                )
                break
        else:
            errors.append(
                f"llms.txt: {len(actual)} section lines, expected {len(expected)}"
                " (extra or missing entries)"
            )


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
    check_llms_txt(config, errors)

    for path in maintained_markdown():
        rel = path.relative_to(ROOT)
        if rel in TRACKER_ALLOWLIST:
            continue
        text = path.read_text(encoding="utf-8")
        for pattern, label in TRACKER_PATTERNS:
            match = pattern.search(text)
            if match is not None:
                line = text.count("\n", 0, match.start()) + 1
                errors.append(
                    f"{rel}:{line}: {label} belongs in kata, not documentation"
                )

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
