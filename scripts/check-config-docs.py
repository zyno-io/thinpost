#!/usr/bin/env python3
"""Fail when docs/reference/configuration.md and the Go code disagree.

Checks, from the repository root:

1. Every "THINPOST_*" string literal in non-test .go files under cmd/ and
   internal/ must appear in the first column of a table in
   docs/reference/configuration.md, and every documented name must be read by
   the code.
2. The names in the "Removed (startup fails)" table must exactly match the
   names internal/controlapp/config.go rejects at startup. They are read by
   the code too, so they are excluded from the rule above.
3. THINPOST_TEST_* names belong to the contributing guide and are ignored.

Standard library only. Exit status is 0 when the two agree, 1 on drift.
"""
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DOC = ROOT / "docs" / "reference" / "configuration.md"
CONFIG_GO = ROOT / "internal" / "controlapp" / "config.go"
NAME = r"THINPOST_[A-Z0-9_]+"
REMOVED_HEADING = re.compile(r"^#+\s+Removed \(startup fails\)\s*$")


def ignored(name):
    return name.startswith("THINPOST_TEST_")


def code_names():
    names = {}
    for top in ("cmd", "internal"):
        for path in sorted((ROOT / top).rglob("*.go")):
            if path.name.endswith("_test.go"):
                continue
            for match in re.finditer(r'"(' + NAME + r')"', path.read_text(encoding="utf-8")):
                if not ignored(match.group(1)):
                    names.setdefault(match.group(1), path.relative_to(ROOT).as_posix())
    return names


def rejected_names():
    """Names in the list literal that config.go loops over and rejects."""
    text = CONFIG_GO.read_text(encoding="utf-8")
    loop = re.search(r"range \[\]string\{([^}]*)\}\s*\{\s*if getenv\(name\) != \"\"", text)
    if not loop:
        sys.exit("check-config-docs: cannot find the rejected-name loop in " + CONFIG_GO.relative_to(ROOT).as_posix())
    return set(re.findall(r'"(' + NAME + r')"', loop.group(1)))


def doc_names():
    documented, removed, duplicates = set(), set(), []
    in_removed = False
    for line in DOC.read_text(encoding="utf-8").splitlines():
        if line.startswith("#"):
            in_removed = bool(REMOVED_HEADING.match(line))
            continue
        row = re.match(r"^\|\s*`(" + NAME + r")`\s*\|", line)
        if not row or ignored(row.group(1)):
            continue
        name = row.group(1)
        if name in documented or name in removed:
            duplicates.append(name)
        (removed if in_removed else documented).add(name)
    return documented, removed, duplicates


def report(title, names):
    if names:
        print(title)
        for name in sorted(names):
            print("  " + name)
    return bool(names)


def main():
    code = code_names()
    rejected = rejected_names()
    documented, removed, duplicates = doc_names()
    expected = set(code) - rejected
    drift = False
    drift |= report("Read by code but missing from the configuration tables:", expected - documented)
    drift |= report("Documented outside the Removed table but not read by code:", documented - expected)
    drift |= report("Rejected by internal/controlapp/config.go but missing from the Removed table:", rejected - removed)
    drift |= report("In the Removed table but not rejected by internal/controlapp/config.go:", removed - rejected)
    drift |= report("Listed more than once in docs/reference/configuration.md:", set(duplicates))
    if drift:
        print("\ndocs/reference/configuration.md is out of date. Update it to match the code.")
        return 1
    print("config docs OK: %d variables documented, %d removed names match config.go" % (len(documented), len(removed)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
