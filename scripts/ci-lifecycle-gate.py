"""Run the lifecycle gate in CI and decide the verdict from the gate's evidence.

scripts/lifecycle-gate.sh runs the acceptance suite. This wrapper filters the
gate's output before it reaches the job log, which is public, reads the
evidence.json that the gate writes, and writes a step summary. It passes only
when the evidence shows every case passing with bound identities and the exit
code agrees with the evidence. While human approval is OPEN, the gate exits 1
even when all cases pass, so a green run is not acceptance.

    TMPDIR=<existing directory> LIFECYCLE_GATE_SENTINEL=<16+ characters> \
        python3 -B scripts/ci-lifecycle-gate.py [--gate PATH]
"""

import argparse
import json
import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
GATE_SCRIPT = ROOT / "scripts" / "lifecycle-gate.sh"
sys.path.insert(0, str(ROOT / "internal" / "lifecyclecheck"))
import gate  # noqa: E402

GATE_TIMEOUT_SECONDS = 1500
MIN_CANARY_LENGTH = 16
MIN_SECRET_LENGTH = 8
SECRET_NAME = re.compile(r"TOKEN|SECRET|KEY|PASS|AUTH")
WITHHELD_LINE = "[line withheld: it contains a protected value]"
WITHHELD_OUTPUT = "[gate output withheld]"
PASS_LINE = "PASS: every case passed and the identities are bound"
LOCAL_LANE = "actual-binary-local"
ADAPTER_LANE = "production-provider-manager"


def expected_cases():
    """Each case ID the gate defines, mapped to the lane that must run it."""
    cases = {name: LOCAL_LANE for name, _ in gate.LOCAL_CASES}
    cases.update({name: ADAPTER_LANE for name, _ in gate.ADAPTER_CASES})
    return cases


def verdict(report, process_exit, cases):
    """Return the problems with an acceptance report; an empty list passes.

    cases maps each expected case ID to its lane.
    """
    problems = []
    if process_exit not in (0, 1):
        problems.append(f"the gate exited {process_exit}")
    if report.get("mode") != "acceptance":
        problems.append("the report is not an acceptance run")
    if report.get("full_suite_selected") is not True:
        problems.append("the full suite was not selected")
    if report.get("lane_complete") is not True:
        problems.append("a required lane did not pass")
    if report.get("identity_bound") is not True:
        problems.append("the source or binary identity is not bound")
    if report.get("exit") != process_exit:
        problems.append("the report exit differs from the gate exit")
    rows = report.get("cases")
    if not isinstance(rows, list):
        problems.append("the report has no case list")
    else:
        seen = []
        for row in rows:
            if not isinstance(row, dict):
                problems.append("a case entry is not an object")
                continue
            name = row.get("id")
            seen.append(name)
            if not isinstance(name, str) or name not in cases:
                problems.append("the report has a case the gate does not define")
            elif row.get("lane") != cases[name]:
                problems.append(f"case {name} ran in the wrong lane")
            if row.get("outcome") != "pass":
                problems.append(f"case {name} did not pass")
        for name in cases:
            count = seen.count(name)
            if count != 1:
                problems.append(f"case {name} is reported {count} times")
    human = report.get("human_approval")
    if human == "proven":
        if process_exit != 0:
            problems.append("human approval is proven but the gate exited nonzero")
    elif isinstance(human, str) and human.startswith("OPEN"):
        if process_exit != 1:
            problems.append("human approval is open but the gate exited 0")
    else:
        problems.append("human approval has an unknown value")
    return problems


def protected_values(environ, canary):
    """The canary, and every long value of a secret-named variable."""
    values = {canary}
    values.update(
        value for name, value in environ.items()
        if SECRET_NAME.search(name) and len(value) >= MIN_SECRET_LENGTH
    )
    return sorted(values)


def scan(text, values):
    """Withhold each line that holds a protected value.

    Return the remaining lines, the number of withheld lines, and whether a
    protected value still shows once the line breaks are removed.
    """
    lines, withheld = [], 0
    for line in text.splitlines():
        if any(value in line for value in values):
            lines.append(WITHHELD_LINE)
            withheld += 1
        else:
            lines.append(line)
    survives = any(value in "".join(lines) for value in values)
    return lines, withheld, survives


def json_strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for key, item in value.items():
            yield key
            yield from json_strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from json_strings(item)


def evidence_leaks(report, values):
    return any(value in text for text in json_strings(report) for value in values)


def decode(output):
    if output is None:
        return ""
    if isinstance(output, bytes):
        return output.decode("utf-8", errors="replace")
    return output


def run_gate(gate_path, timeout):
    """Run the gate. Return its output and exit code, or None as the code on timeout."""
    try:
        done = subprocess.run(
            [gate_path],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=timeout,
        )
    except subprocess.TimeoutExpired as exc:
        return decode(exc.output), None
    return done.stdout, done.returncode


def evidence_dirs(temp):
    return {path.name for path in temp.glob("flats-lifecycle-gate.*") if path.is_dir()}


def read_evidence(temp, before, reasons):
    """Return the one new evidence report, or None and a reason when there is none."""
    found = sorted(
        temp / name / "evidence.json"
        for name in evidence_dirs(temp) - before
        if (temp / name / "evidence.json").is_file()
    )
    if len(found) != 1:
        reasons.append(f"found {len(found)} new evidence files, expected one")
        return None
    try:
        report = json.loads(found[0].read_text(encoding="utf-8"))
    except (OSError, ValueError):
        reasons.append("evidence.json is unreadable")
        return None
    if not isinstance(report, dict):
        reasons.append("evidence.json is not a JSON object")
        return None
    return report


def summary_text(report):
    if report is None:
        return ""
    rows = report.get("cases")
    rows = [row for row in rows if isinstance(row, dict)] if isinstance(rows, list) else []
    lines = ["### Lifecycle gate", "", "| Case | Lane | Outcome |", "| --- | --- | --- |"]
    lines += [f"| {row.get('id')} | {row.get('lane')} | {row.get('outcome')} |" for row in rows]
    failed = next((row for row in rows if row.get("outcome") == "fail"), None)
    if failed is not None:
        lines += ["", f"First failure, {failed.get('id')}: {failed.get('error')}"]
    return "\n".join(lines)


def conclude(reasons, summary, summary_path, values):
    """Write the step summary, print the verdict and return the exit code."""
    passed = not reasons
    line = PASS_LINE if passed else "FAIL: " + "; ".join(reasons)
    if any(value in line for value in values):
        passed, line = False, "FAIL: leak"
    if summary_path:
        with open(summary_path, "a", encoding="utf-8") as handle:
            handle.write((summary + "\n\n" if summary else "") + line + "\n")
    print(line)
    return 0 if passed else 1


def main(argv=None, timeout=GATE_TIMEOUT_SECONDS):
    parser = argparse.ArgumentParser(description="Run the lifecycle gate and decide the CI verdict.")
    parser.add_argument("--gate", default=str(GATE_SCRIPT), help="gate executable to run")
    args = parser.parse_args(argv)
    environ = os.environ
    summary_path = environ.get("GITHUB_STEP_SUMMARY")
    temp = environ.get("TMPDIR", "")
    canary = environ.get("LIFECYCLE_GATE_SENTINEL", "")
    if not temp or not Path(temp).is_dir():
        return conclude(["TMPDIR does not name an existing directory"], "", summary_path, [])
    if len(canary) < MIN_CANARY_LENGTH:
        return conclude(
            [f"LIFECYCLE_GATE_SENTINEL has fewer than {MIN_CANARY_LENGTH} characters"],
            "", summary_path, [],
        )
    values = protected_values(environ, canary)
    temp_dir = Path(temp)
    before = evidence_dirs(temp_dir)
    raw, process_exit = run_gate(args.gate, timeout)
    reasons = []
    if process_exit is None:
        reasons.append(f"the gate did not finish within {timeout} seconds")
    lines, withheld, leaked = scan(raw, values)
    if leaked:
        print(WITHHELD_OUTPUT)
    else:
        for line in lines:
            print(line)
    report = None
    if process_exit is not None:
        report = read_evidence(temp_dir, before, reasons)
    if report is not None:
        leaked = leaked or evidence_leaks(report, values)
        reasons.extend(verdict(report, process_exit, expected_cases()))
    body, body_withheld, body_leaked = scan(summary_text(report), values)
    summary = WITHHELD_OUTPUT if body_leaked else "\n".join(body)
    if withheld or body_withheld:
        reasons.append("a line was withheld")
    if leaked or body_leaked:
        reasons.append("leak")
    return conclude(reasons, summary, summary_path, values)


if __name__ == "__main__":
    raise SystemExit(main())
