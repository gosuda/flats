from __future__ import annotations

from dataclasses import asdict, dataclass
from hashlib import sha256
from pathlib import Path
import json
import os
import re
import subprocess


ROOT = Path.cwd()
PACKAGE = ROOT / "internal/notebook"
OUTPUT = ROOT / "memory-oracle-evidence"
FILES = ("markdown_read.go", "reader_query.go", "reader_entry_test.go", "markdown_read_memory_test.go")
ORIGINAL = {name: (PACKAGE / name).read_text() for name in FILES}
SELECTED = "^TestMarkdownRetainedMemory$/^option_schema$"


@dataclass(frozen=True)
class Result:
    name: str
    status: int
    expectation_met: bool
    log_sha256: str


def replace_once(source: str, old: str, new: str) -> str:
    assert source.count(old) == 1, (old, source.count(old))
    return source.replace(old, new, 1)


def restore() -> None:
    for name, source in ORIGINAL.items():
        (PACKAGE / name).write_text(source)


def write(name: str, source: str) -> None:
    (PACKAGE / name).write_text(source)
    subprocess.run(["gofmt", "-w", str(PACKAGE / name)], check=True)


def old_oracle() -> None:
    source = ORIGINAL["markdown_read_memory_test.go"]
    write("markdown_read_memory_test.go", replace_once(
        source,
        "body, peak, err = measureReaderLiveHeap(t, f.in, raw)",
        "body, _, peak, err = measureReaderHeap(t, f.in, raw)",
    ))


def retain_option_ids() -> None:
    source = ORIGINAL["markdown_read.go"]
    source = replace_once(source, '\t"io"\n', '\t"io"\n\t"runtime"\n')
    source = replace_once(source, "func (c *markdownReadCapture) database(id ID) error {\n", """func (c *markdownReadCapture) database(id ID) error {
	var optionCount int
	if err := c.tx.QueryRowContext(c.ctx, `SELECT count(*) FROM options o JOIN properties p ON p.id=o.property_id WHERE p.database_id=?`, id).Scan(&optionCount); err != nil {
		return err
	}
	retainedOptionIDs := make([]ID, 0, optionCount)
	defer func() { runtime.KeepAlive(retainedOptionIDs) }()
""")
    source = replace_once(source, "\t\treturn spillOrder(c.ctx, c.tx, pid, OptionOrderGroup, func(oid ID) error {\n", "\t\treturn spillOrder(c.ctx, c.tx, pid, OptionOrderGroup, func(oid ID) error {\n\t\t\tretainedOptionIDs = append(retainedOptionIDs, oid)\n")
    write("markdown_read.go", source)


def retain_preparation_buffer(instrument: bool) -> None:
    source = ORIGINAL["reader_query.go"]
    source = replace_once(source, '\t"os"\n', '\t"os"\n\t"runtime"\n\t"sync/atomic"\n')
    source = replace_once(source, "const spillRankSlot = 40\n", "const spillRankSlot = 40\n\nvar memoryProbePreparation atomic.Bool\n")
    source = replace_once(source, "\tc.rankSlots = 1\n", """	memoryProbePreparation.Store(true)
	preparationBuffer := make([]byte, count*32)
	defer func() {
		runtime.KeepAlive(preparationBuffer)
		memoryProbePreparation.Store(false)
	}()
	rows, err := tx.QueryContext(ctx, `SELECT o.id FROM options o JOIN properties p ON p.id=o.property_id WHERE p.database_id=?`, database)
	if err != nil {
		return err
	}
	defer rows.Close()
	position := 0
	for rows.Next() {
		var optionID ID
		if err := rows.Scan(&optionID); err != nil {
			return err
		}
		copy(preparationBuffer[position:position+32], optionID)
		position += 32
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	c.rankSlots = 1
""")
    write("reader_query.go", source)
    if instrument:
        helper = ORIGINAL["reader_entry_test.go"]
        helper = replace_once(helper, "\tvar periodic int\n", "\tvar periodic, preparationSamples int\n")
        helper = replace_once(helper, "\t\t\t\tsample()\n\t\t\t\tperiodic++\n", "\t\t\t\tpreparing := memoryProbePreparation.Load()\n\t\t\t\tsample()\n\t\t\t\tif preparing && memoryProbePreparation.Load() {\n\t\t\t\t\tpreparationSamples++\n\t\t\t\t}\n\t\t\t\tperiodic++\n")
        helper = replace_once(helper, '\tt.Logf("collected live-heap periodic samples=%d", periodic)\n', '\tt.Logf("collected live-heap periodic samples=%d", periodic)\n\tt.Logf("pre-output periodic collections=%d", preparationSamples)\n')
        write("reader_entry_test.go", helper)


def retain_over_ceiling() -> None:
    source = ORIGINAL["markdown_read.go"]
    source = replace_once(source, '\t"io"\n', '\t"io"\n\t"runtime"\n')
    source = replace_once(source, "func (c *markdownReadCapture) database(id ID) error {\n", """func (c *markdownReadCapture) database(id ID) error {
	retained := make([]byte, 6<<20)
	retained[0] = 1
	defer func() { runtime.KeepAlive(retained) }()
""")
    write("markdown_read.go", source)


def run(name: str, expected: str, pattern: str = SELECTED, race: bool = False) -> Result:
    command = ["go", "test", "-v", "-count=1", "-timeout=20m"]
    if race:
        command.append("-race")
    command += ["./internal/notebook", "-run", pattern]
    log_path = OUTPUT / f"{name}.log"
    print(json.dumps({"starting": name, "command": command, "load": os.getloadavg()}), flush=True)
    with log_path.open("w") as log:
        completed = subprocess.run(command, stdout=log, stderr=subprocess.STDOUT, timeout=1250, check=False)
    text = log_path.read_text()
    growth = "exceeds 1 MiB" in text
    ceiling = "exceeds 4 MiB" in text
    if expected == "pass":
        accepted = completed.returncode == 0 and "--- PASS: TestMarkdownRetainedMemory" in text
    elif expected == "growth":
        accepted = completed.returncode != 0 and growth and not ceiling and "--- FAIL: TestMarkdownRetainedMemory/option_schema/bounded_growth" in text
    elif expected == "ceiling":
        accepted = completed.returncode != 0 and ceiling
    else:
        raise AssertionError(expected)
    if name == "preparation-new":
        match = re.search(r"=== RUN   TestMarkdownRetainedMemory/option_schema/16MiB_live\n(.*?)(?==== RUN|\Z)", text, re.S)
        assert match is not None, "missing 16 MiB live case"
        counts = re.findall(r"pre-output periodic collections=(\d+)", match.group(1))
        accepted = accepted and any(int(count) > 0 for count in counts)
    result = Result(name, completed.returncode, accepted, sha256(log_path.read_bytes()).hexdigest())
    print(json.dumps(asdict(result)), flush=True)
    for line in text.splitlines():
        if any(marker in line for marker in ("sampled", "collected live-heap", "pre-output", "exceeds", "--- FAIL:", "FAIL\t", "ok  \t")):
            print(line, flush=True)
    if not accepted:
        print(text[-18000:], flush=True)
    return result


def main() -> None:
    OUTPUT.mkdir()
    source = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    (OUTPUT / "source.txt").write_text(source + "\n")
    (OUTPUT / "source-hashes.json").write_text(json.dumps({name: sha256(value.encode()).hexdigest() for name, value in ORIGINAL.items()}, indent=2) + "\n")
    results: list[Result] = []
    try:
        baseline = run("baseline", "pass", "^TestMarkdownRetainedMemory$")
        results.append(baseline)
        if not baseline.expectation_met:
            raise AssertionError("the unmodified candidate failed")
        for name in ("option-ids", "preparation"):
            for oracle in ("old", "new"):
                restore()
                if name == "preparation":
                    retain_preparation_buffer(oracle == "new")
                else:
                    retain_option_ids()
                if oracle == "old":
                    old_oracle()
                results.append(run(f"{name}-{oracle}", "pass" if oracle == "old" else "growth"))
        restore()
        retain_over_ceiling()
        results.append(run("absolute-ceiling", "ceiling"))
    finally:
        restore()
        assert all((PACKAGE / name).read_text() == value for name, value in ORIGINAL.items())
        (OUTPUT / "results.json").write_text(json.dumps([asdict(result) for result in results], indent=2) + "\n")
    results.append(run("restored", "pass", "^TestMarkdownRetainedMemory$"))
    results.append(run("race", "pass", race=True))
    (OUTPUT / "results.json").write_text(json.dumps([asdict(result) for result in results], indent=2) + "\n")
    assert all(result.expectation_met for result in results), "one or more oracle checks failed"


if __name__ == "__main__":
    main()
