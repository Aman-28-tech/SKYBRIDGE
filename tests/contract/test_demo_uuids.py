"""Regression test: every deterministic demo UUID must be RFC-4122-compatible.

Guards the demo-migration.sh step-5 bug where
  DEMO_USER="aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa${RUN_ID: -4}"
produced "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaamo01" for DEMO_RUN_ID=labdemo01
(non-hex "mo" suffix -> PostgreSQL "invalid input syntax for type uuid").

The fixed derivation embeds sha256(RUN_ID) hex digest material (hex-only
by construction) into a v4 template with fixed version/variant nibbles.
This test replicates that derivation for hostile RUN_IDs and strictly
parses the result, scans the scripts for the buggy pattern, and parses
every hardcoded UUID literal in the demo surface.

No live services required. Run: python3 -m pytest tests/contract -q
"""
import hashlib
import re
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
DEMO_SCRIPT = ROOT / "scripts" / "demo-migration.sh"
ACCEPTANCE_SCRIPT = ROOT / "scripts" / "acceptance-local-multicloud.sh"
FAILURES_SCRIPT = ROOT / "scripts" / "demo-failures.sh"
DEMO_FIXTURES = ROOT / "tests" / "fixtures" / "demo"

# Strict RFC-4122 v4: hex-only, version nibble 4, variant nibble 8/9/a/b.
V4_RE = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
# Postgres uuid type accepts any 8-4-4-4-12 hex (variant not enforced).
PG_UUID_RE = re.compile(
    r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
    r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
)
UUID_LITERAL_RE = re.compile(
    r"[0-9a-zA-Z]{8}-[0-9a-zA-Z]{4}-[0-9a-zA-Z]{4}-"
    r"[0-9a-zA-Z]{4}-[0-9a-zA-Z]{12}"
)


def demo_user_for(run_id: str) -> str:
    """Mirror scripts/demo-migration.sh DEMO_USER derivation exactly."""
    suffix = hashlib.sha256(run_id.encode()).hexdigest()[:4]
    return f"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa{suffix}"


def test_buggy_pattern_removed_from_demo_script():
    text = DEMO_SCRIPT.read_text()
    # The raw RUN_ID tail must never be interpolated into a UUID again.
    assert "${RUN_ID: -4}" not in text, (
        "demo-migration.sh still interpolates raw RUN_ID tail into DEMO_USER"
    )
    assert "aaaaaaaa${RUN_ID" not in text, (
        "demo-migration.sh still builds a UUID from raw RUN_ID material"
    )


def test_demo_user_valid_for_hostile_run_ids():
    hostile = [
        "labdemo01",  # the exact failing input (tail "mo01")
        "demo01",
        "cloud01",
        "DEMO01",
        "xyz",
        "1",
        "1234567890",  # default timestamp-style RUN_ID
        "-direct-",
        "aaaaaaaa",
        "mo01",
        "ff" * 100,  # long input
    ]
    for run_id in hostile:
        user = demo_user_for(run_id)
        assert V4_RE.match(user), f"RUN_ID={run_id!r} -> invalid {user!r}"
        parsed = uuid.UUID(user)  # strict stdlib parse (raises on bad input)
        assert parsed.version == 4, f"{user} parsed as v{parsed.version}"
        assert set(user.replace("-", "")) <= set("0123456789abcdef"), user


def test_demo_user_deterministic_and_distinct():
    assert demo_user_for("labdemo01") == demo_user_for("labdemo01")
    assert demo_user_for("labdemo01") != demo_user_for("labdemo02")
    # Exact value pinned: guards silent derivation drift across resets.
    assert demo_user_for("labdemo01") == "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaa22df"


def test_hardcoded_uuid_literals_parse():
    paths = [DEMO_SCRIPT, ACCEPTANCE_SCRIPT, FAILURES_SCRIPT] + sorted(
        DEMO_FIXTURES.glob("*.json")
    )
    found = []
    for path in paths:
        for match in UUID_LITERAL_RE.finditer(path.read_text()):
            found.append((str(path), match.group(0)))
    assert found, "expected at least one UUID literal in the demo surface"
    for path, literal in found:
        assert PG_UUID_RE.match(literal), f"{path}: non-hex UUID {literal!r}"
        uuid.UUID(literal)  # raises ValueError on invalid syntax
