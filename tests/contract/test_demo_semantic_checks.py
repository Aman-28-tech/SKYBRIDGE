"""Regression test: demo scripts must parse order JSON semantically.

CloudShop serves fresh writes as Go-compact JSON ('"status":"pending"')
but idempotency replays from Postgres jsonb, which formats with a space
('"status": "pending"'). Byte-grepping for the compact form therefore
fails deterministically on every re-run that reuses a RUN_ID (replay path),
even though the order is pending. Checks on replay-capable paths must
parse JSON and assert the status field.

Run: python3 -m pytest tests/contract -q
"""
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
SCRIPTS = [
    ROOT / "scripts" / "demo-migration.sh",
    ROOT / "scripts" / "acceptance-local-multicloud.sh",
    ROOT / "scripts" / "demo-failures.sh",
]


def test_no_byte_exact_pending_grep_on_replay_paths():
    for path in SCRIPTS:
        for lineno, line in enumerate(path.read_text().splitlines(), 1):
            stripped = line.strip()
            if stripped.startswith("#"):
                continue
            assert 'grep -q' not in line or '"status"' not in line, (
                f"{path.name}:{lineno}: byte-exact status grep breaks on "
                f"jsonb replay formatting; parse JSON instead: {line.strip()}"
            )


def test_rehearsal_keys_carry_invocation_nonce():
    """Rehearsal probe IDs and the Redpanda consumer group derive from the
    rehearsal HTTP idempotency key, and broker groups persist across demo
    invocations. A reused rehearsal key makes awaitProbes match the previous
    invocation's probe records (same IDs) and report genuinely stale lag.
    Every /rehearse key in the demo scripts must therefore embed the
    per-invocation $INV nonce. The scenario-J concurrent cutover keys are
    the deliberate exception (identical keys prove exactly-once)."""
    for path in SCRIPTS:
        for lineno, line in enumerate(path.read_text().splitlines(), 1):
            stripped = line.strip()
            if stripped.startswith("#") or "/rehearse" not in line:
                continue
            assert "$INV" in line, (
                f"{path.name}:{lineno}: /rehearse key without $INV nonce "
                f"reuses probe IDs/groups across invocations: {stripped[:120]}"
            )
