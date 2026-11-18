#!/usr/bin/env python3
"""Fail when docs/reference/helm-values.md and the chart schema disagree.

Walks every property path of resources/helm/chart/values.schema.json (nested
objects use dots, list items use "[]", for example additionalSources[].address)
and requires each one as a code span in the first column of a table in
docs/reference/helm-values.md. A documented path that is not in the schema is
also reported.

Standard library only. Run from anywhere; exit status is 0 when they agree
and 1 on drift.
"""
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCHEMA = ROOT / "resources" / "helm" / "chart" / "values.schema.json"
DOC = ROOT / "docs" / "reference" / "helm-values.md"


def walk(node, prefix, out):
    """Collect property paths below a schema node."""
    for name, child in node.get("properties", {}).items():
        path = prefix + name
        out.add(path)
        walk(child, path + ".", out)
        items = child.get("items")
        if isinstance(items, dict) and items.get("properties"):
            walk(items, path + "[].", out)


def doc_paths():
    paths = set()
    for line in DOC.read_text(encoding="utf-8").splitlines():
        row = re.match(r"^\|\s*`([A-Za-z0-9_.\[\]-]+)`\s*\|", line)
        if row:
            paths.add(row.group(1))
    return paths


def main():
    schema = set()
    walk(json.loads(SCHEMA.read_text(encoding="utf-8")), "", schema)
    documented = doc_paths()
    missing = schema - documented
    extra = documented - schema
    if missing:
        print("In values.schema.json but missing from docs/reference/helm-values.md:")
        for path in sorted(missing):
            print("  " + path)
    if extra:
        print("Documented in docs/reference/helm-values.md but not in values.schema.json:")
        for path in sorted(extra):
            print("  " + path)
    if missing or extra:
        print("\ndocs/reference/helm-values.md is out of date. Update it to match the chart schema.")
        return 1
    print("helm values docs OK: %d schema paths documented" % len(schema))
    return 0


if __name__ == "__main__":
    sys.exit(main())
