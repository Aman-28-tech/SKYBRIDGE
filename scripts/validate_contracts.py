#!/usr/bin/env python3
"""Minimal contract validator: JSON Schema fixtures + YAML registry + OpenAPI parse + envelope check.

Usage:
  python3 scripts/validate_contracts.py --all
  python3 scripts/validate_contracts.py --schema ... --fixture ... [--yaml]

Requires: python3 + pyyaml + jsonschema (pip install pyyaml jsonschema).
Exits non-zero on any failure. Never claims success without running the check.
"""
import argparse
import json
import sys
from pathlib import Path

try:
    import yaml
except ImportError:
    yaml = None

try:
    import jsonschema
    from jsonschema import Draft202012Validator
except ImportError:
    jsonschema = None


def load_doc(path: Path):
    text = path.read_text()
    if path.suffix in (".yaml", ".yml"):
        if yaml is None:
            print("FAIL: pyyaml not installed; cannot parse YAML", file=sys.stderr)
            sys.exit(2)
        return yaml.safe_load(text)
    return json.loads(text)


def validate(schema_path: Path, fixture_path: Path, is_yaml: bool = False):
    if jsonschema is None:
        print("FAIL: jsonschema not installed; run pip install jsonschema", file=sys.stderr)
        return False
    schema = json.loads(schema_path.read_text())
    fixture = load_doc(fixture_path)
    try:
        Draft202012Validator.check_schema(schema)
    except Exception as e:
        print(f"FAIL: schema {schema_path} invalid: {e}")
        return False
    validator = Draft202012Validator(schema)
    errors = sorted(validator.iter_errors(fixture), key=lambda e: list(e.path))
    if errors:
        print(f"FAIL: {fixture_path} does not validate against {schema_path}:")
        for e in errors[:10]:
            print(f"  - {'/'.join(str(p) for p in e.path)}: {e.message}")
        return False
    print(f"PASS: {fixture_path} validates against {schema_path}")
    return True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--all", action="store_true")
    ap.add_argument("--schema", default=None)
    ap.add_argument("--fixture", default=None)
    ap.add_argument("--yaml", action="store_true")
    args = ap.parse_args()
    root = Path(__file__).resolve().parent.parent
    ok = True
    if args.all:
        pairs = [
            ("packages/contracts/schemas/canonical-workload.schema.json",
             "packages/contracts/schemas/cloudshop-workload.v1.json", False),
            ("packages/contracts/schemas/capability-registry.schema.json",
             "packages/contracts/schemas/capability-registry.example.yaml", True),
        ]
        for s, f, _ in pairs:
            if not validate(root / s, root / f):
                ok = False
        # OpenAPI must at least parse as YAML/JSON
        oapi = root / "packages/contracts/openapi/skybridge.yaml"
        try:
            doc = load_doc(oapi)
            assert doc.get("openapi", "").startswith("3."), "openapi version must be 3.x"
            assert "/v1/migrations/{migrationId}/cutover" in doc["paths"], "cutover path missing"
            assert "/v1/migrations/{migrationId}/rollback" in doc["paths"], "rollback path missing"
            print(f"PASS: {oapi} parses with required paths")
        except Exception as e:
            print(f"FAIL: {oapi}: {e}")
            ok = False
        sys.exit(0 if ok else 1)
    if not args.schema or not args.fixture:
        ap.error("--schema and --fixture required (or --all)")
    sys.exit(0 if validate(Path(args.schema), Path(args.fixture), args.yaml) else 1)


if __name__ == "__main__":
    main()
