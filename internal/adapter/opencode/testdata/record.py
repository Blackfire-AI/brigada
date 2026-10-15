"""Refresh JSONL fixtures using the real CLI and an offline SDK test provider.

Run from the worktree root: python3 internal/adapter/opencode/testdata/record.py
Requires opencode 1.19.0 on PATH. No provider credentials or network are used.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

fixtures = Path(__file__).resolve().parent
binary = shutil.which("opencode")
if not binary:
    raise SystemExit("opencode is not on PATH")
version = subprocess.check_output([binary, "--version"], text=True).strip()
if version != "1.19.0":
    raise SystemExit(f"expected opencode 1.19.0, got {version}")

# Keep the CLI's databases, config and caches inside this worktree; never
# read the user's auth store or use a real provider account to make errors.
with tempfile.TemporaryDirectory(prefix=".opencode-record-", dir=Path.cwd()) as name:
    work = Path(name)
    (work / "sample.txt").write_text("fixture input\n")
    config = {
        "enabled_providers": ["fixture"],
        "provider": {"fixture": {
            "npm": (fixtures / "provider.mjs").as_uri(),
            "name": "Fixture",
            "models": {"fixture": {
                "name": "Fixture", "limit": {"context": 32000, "output": 1000},
            }},
        }},
        "autoupdate": False,
        "share": "disabled",
        "snapshot": False,
        "small_model": "fixture/fixture",
    }
    env = {
        "PATH": os.environ["PATH"],
        "XDG_DATA_HOME": str(work / "data"),
        "XDG_CONFIG_HOME": str(work / "config"),
        "XDG_CACHE_HOME": str(work / "cache"),
        "XDG_STATE_HOME": str(work / "state"),
        "OPENCODE_CONFIG_CONTENT": json.dumps(config),
        "OPENCODE_DISABLE_MODELS_FETCH": "true",
        "OPENCODE_DISABLE_DEFAULT_PLUGINS": "true",
        "FIXTURE_FILE": str(work / "sample.txt"),
    }
    for mode, filename in [
        ("normal", "normal.jsonl"),
        ("limit", "failure-limit.jsonl"),
        ("auth", "failure-auth.jsonl"),
    ]:
        env["FIXTURE_MODE"] = mode
        result = subprocess.run(
            [binary, "--pure", "run", "--format", "json", "-m", "fixture/fixture",
             "--", "Read sample.txt and give the final answer."],
            cwd=work, env=env, capture_output=True, text=True, timeout=35,
        )
        expected_exit = 0 if mode == "normal" else 1
        if result.returncode != expected_exit or not result.stdout.strip():
            raise SystemExit(f"{mode}: exit {result.returncode}: {result.stderr}")
        data = result.stdout
        for line in result.stdout.splitlines():
            event = json.loads(line)
            data = data.replace(event["sessionID"], "ses_fixture")
        data = data.replace(str(work), "/fixture")
        data = data.replace(work.name + "/sample.txt", "sample.txt")
        (fixtures / filename).write_text(data)
        print(f"Recorded {filename}")
