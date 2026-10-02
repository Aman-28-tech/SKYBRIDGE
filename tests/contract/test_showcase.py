"""Static contract for the portfolio/demo-experience scripts.

Guards: no live-mutation strings in any script (mirrors the showcase
CLOUD_GATE and acceptance NO_CLOUD_CALLS magnets, including the
comment-line exclusion for prohibition comments), showcase step order,
demo phase banners, and the failures table header.
Run: python3 -m pytest tests/contract -q
"""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
SCRIPTS = ROOT / "scripts"


def _code_lines(path: Path):
    """Non-comment, non-empty lines (prohibition comments are not calls)."""
    out = []
    for line in path.read_text().splitlines():
        s = line.strip()
        if not s or s.startswith("#"):
            continue
        out.append(line)
    return out


def test_no_cloud_mutation_strings_in_scripts():
    sdk = re.compile(r"aws-sdk-go|azure-sdk|management\.azure\.com|sts\.amazonaws\.com")
    apply = re.compile(r"terraform (apply|destroy)")
    hits = []
    for path in sorted(SCRIPTS.glob("*.sh")):
        for line in _code_lines(path):
            # Guard lines grep FOR these strings to assert their absence;
            # they are the check itself, not a usage.
            if "grep " in line:
                continue
            if sdk.search(line) or apply.search(line):
                hits.append(f"{path.name}: {line.strip()[:100]}")
    assert not hits, f"live-mutation strings in executable lines: {hits}"


def test_no_secret_shapes_in_new_scripts():
    secret = re.compile(
        r"AKIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY"
        r"|aws_secret_access_key|azure.*client.?secret",
        re.IGNORECASE,
    )
    for name in ["skybridge-status.sh", "doctor.sh", "test-all.sh", "showcase.sh"]:
        text = (SCRIPTS / name).read_text()
        assert not secret.search(text), f"secret shape in {name}"


def test_showcase_step_order_and_verdict():
    text = (SCRIPTS / "showcase.sh").read_text()
    steps = ["doctor.sh", "reset-demo.sh", "demo-migration.sh",
             "demo-failures.sh", "reset-demo.sh", "skybridge-status.sh"]
    positions = []
    cursor = 0
    for s in steps:
        pos = text.index(s, cursor)
        positions.append(pos)
        cursor = pos + 1
    assert positions == sorted(positions), "showcase step order changed"
    assert "SKYBRIDGE_SHOWCASE = PASS" in text


def test_demo_phase_banners():
    text = (SCRIPTS / "demo-migration.sh").read_text()
    for group in ["SETUP", "WORKLOAD", "CDC", "VALIDATION", "REHEARSAL",
                  "CANARY", "CUTOVER", "POST-CUTOVER", "EVIDENCE"]:
        assert f'phase "{group}"' in text, f"missing [{group}] banner"
    for line in ["SOURCE        AWS", "TARGET        AZURE",
                 "AUTHORITY     AZURE", "CUTOVER       COMPLETE",
                 "AZURE WRITE   PASS", "AWS WRITE     REJECTED",
                 "SPLIT BRAIN   PREVENTED"]:
        assert line in text, f"missing summary line: {line}"


def test_failures_table_header():
    text = (SCRIPTS / "demo-failures.sh").read_text()
    assert "SCENARIO   FAILURE" in text and "RESULT" in text
    assert "trap finish EXIT" in text, "table must print on failure paths too"
