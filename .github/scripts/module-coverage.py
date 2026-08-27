#!/usr/bin/env python3
"""Check that every Go module is watched by Dependabot and can be released.

Both lists this checks are hand-kept and cannot be derived: neither
`.github/dependabot.yml` nor `on.push.tags` in a workflow accepts expressions.
This repository has found each of them missing an entry once already —
`receivers/http` unwatched from M3, and `receivers/*` unreleasable until the M6
audit — and in both cases the symptom was silence.

So the lists stay hand-kept and the omission stops being silent.

Usage: module-coverage.py '<modules json>' '<test-only json>'
"""

import json
import pathlib
import re
import sys


def tag_patterns(workflow: str) -> list[str]:
    """The `on.push.tags` globs, read without a YAML parser.

    Runners are not guaranteed to have PyYAML, and this file is small and
    regular enough that a parser buys nothing.
    """
    text = pathlib.Path(workflow).read_text()
    block = re.search(r"\n\s*tags:\n((?:\s*-\s*\"[^\"]+\"\n)+)", text)
    if not block:
        return []
    return re.findall(r'-\s*"([^"]+)"', block.group(1))


def matches(pattern: str, tag: str) -> bool:
    """Whether a GitHub tag glob matches a tag.

    `*` does not cross a `/`; `**` does. Getting that distinction wrong is not
    academic — the first version of this function expanded `**` as two `*`,
    which meant a pattern of `exporters/**/v*` would have been reported as
    unable to reach `exporters/otlp/integration`, exactly the case the
    test-only assertion below exists to catch.
    """
    regex, i = "", 0
    while i < len(pattern):
        if pattern.startswith("**", i):
            regex += ".*"
            i += 2
        elif pattern[i] == "*":
            regex += "[^/]*"
            i += 1
        else:
            regex += re.escape(pattern[i])
            i += 1
    return re.fullmatch(regex, tag) is not None


def dependabot_directories(config: str) -> set[str]:
    text = pathlib.Path(config).read_text()
    return {d.strip("/") for d in re.findall(r'directory:\s*"([^"]+)"', text)}


def main() -> int:
    modules = json.loads(sys.argv[1])
    test_only = set(json.loads(sys.argv[2]))

    watched = dependabot_directories(".github/dependabot.yml")
    patterns = tag_patterns(".github/workflows/release.yml")
    if not patterns:
        print("::error::found no tag patterns in release.yml; the check did not run")
        return 1

    failures = 0
    for module in modules:
        released = [p for p in patterns if matches(p, f"{module}/v0.1.0")]

        if module in test_only:
            # A test-only module is never published. It needs no Dependabot
            # entry, and it must not be reachable by a release tag.
            if released:
                print(f"::error::{module} is test-only but matches release pattern(s) "
                      f"{released}; it could be published by tagging it")
                failures += 1
            else:
                print(f"  {module}: test-only, not watched, not releasable")
            continue

        if module not in watched:
            print(f"::error::{module} has no entry in .github/dependabot.yml, so its "
                  f"dependencies are not watched. Add one, or add it to TEST_ONLY in "
                  f"the discover job if it is never published.")
            failures += 1
        if not released:
            print(f"::error::no tag pattern in release.yml matches {module}/v*, so it "
                  f"cannot be released. Add one.")
            failures += 1
        if module in watched and released:
            print(f"  {module}: watched, releasable via {released[0]}")

    if failures:
        print(f"::error::{failures} module coverage problem(s)")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
