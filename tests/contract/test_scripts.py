"""Script portability tests (no cloud, no Terraform providers required).

Proves scripts/tf-plan.sh honors TERRAFORM_BIN using a stub binary that
records its invocations, and fails clearly when no binary exists.
Run: python3 -m pytest tests/contract/test_scripts.py -q
"""
import os
import stat
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
SCRIPT = ROOT / "scripts" / "tf-plan.sh"

STUB = """#!/usr/bin/env bash
echo "$@" >> "$SKYBRIDGE_STUB_LOG"
if [ "$1" = "fmt" ]; then echo "FMT_CLEAN"; fi
if [ "$1" = "-chdir" ]; then echo "Success! The configuration is valid."; fi
exit 0
"""


def _write_stub(tmp: Path) -> Path:
    stub = tmp / "terraform-stub"
    stub.write_text(STUB)
    stub.chmod(stub.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    (tmp / "calls.log").write_text("")
    return stub


def test_tf_plan_uses_terraform_bin_override(tmp_path):
    stub = _write_stub(tmp_path)
    log = tmp_path / "calls.log"
    env = dict(os.environ, TERRAFORM_BIN=str(stub), SKYBRIDGE_STUB_LOG=str(log))
    env.pop("TF", None)
    proc = subprocess.run(["bash", str(SCRIPT)], capture_output=True, text=True, env=env, timeout=120)
    assert proc.returncode == 0, proc.stderr
    assert "FMT_CLEAN" in proc.stdout
    assert "ALL_VALIDATE_OK" in proc.stdout
    calls = log.read_text()
    assert "fmt -check -recursive" in calls
    assert "validate" in calls
    # apply/destroy must never be invoked by the script
    assert "apply" not in calls
    assert "destroy" not in calls


def test_tf_plan_legacy_tf_var_still_works(tmp_path):
    stub = _write_stub(tmp_path)
    log = tmp_path / "calls.log"
    env = dict(os.environ, TF=str(stub), SKYBRIDGE_STUB_LOG=str(log))
    env.pop("TERRAFORM_BIN", None)
    proc = subprocess.run(["bash", str(SCRIPT)], capture_output=True, text=True, env=env, timeout=120)
    assert proc.returncode == 0, proc.stderr
    assert "ALL_VALIDATE_OK" in proc.stdout


def test_tf_plan_fails_clearly_without_binary(tmp_path):
    env = dict(os.environ, TERRAFORM_BIN="/nonexistent/terraform-binary")
    env.pop("TF", None)
    proc = subprocess.run(["bash", str(SCRIPT)], capture_output=True, text=True, env=env, timeout=60)
    assert proc.returncode == 127
    assert "terraform executable not found" in proc.stderr
    assert "TERRAFORM_BIN" in proc.stderr
