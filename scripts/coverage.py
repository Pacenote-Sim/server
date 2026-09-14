#!/usr/bin/env python3
"""Report statement coverage per package, and fail if any package is under the bar.

Coverage has to be measured across the whole suite rather than package by
package. Most of internal/db is exercised by the admin and api tests, and
`go test -cover ./internal/db/` counts none of that — it reported 55% for a
package the suite actually covers to 76%. So the profile is produced with
-coverpkg over everything, which makes every test binary emit its own copy of
every block; this merges them by taking the highest count for each block.

Two kinds of package are excluded, and both are named rather than pattern
matched so that adding one is a decision:

  internal/db/gen   sqlc output. Writing tests against generated code tests sqlc.
  *test helpers     packages that exist only to be used by tests.

Usage:
    scripts/coverage.py [--profile FILE] [--min 90] [--quiet]
"""

import argparse
import collections
import re
import sys

EXCLUDED = {
    "internal/db/gen",
    "internal/db/dbtest",
    "internal/clientbuild/clientbuildtest",
}

MODULE = "github.com/pacenote-sim/server/"


def merge(path):
    """Read a coverprofile, keeping the highest count seen for each block."""
    blocks = {}
    with open(path) as f:
        header = next(f)
        if not header.startswith("mode:"):
            raise SystemExit(f"{path} is not a coverage profile")
        for line in f:
            line = line.strip()
            if not line:
                continue
            name, stmts, count = line.rsplit(" ", 2)
            n, c = int(stmts), int(count)
            prev = blocks.get(name)
            if prev is None or c > prev[1]:
                blocks[name] = (n, c)
    return blocks


def by_package(blocks):
    per = collections.defaultdict(lambda: [0, 0])  # covered, total
    for name, (n, c) in blocks.items():
        path = name.split(":")[0].removeprefix(MODULE)
        pkg = "/".join(path.split("/")[:-1])
        per[pkg][1] += n
        if c > 0:
            per[pkg][0] += n
    return per


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--profile", default="coverage.out")
    ap.add_argument("--min", type=float, default=90.0,
                    help="the floor every package has to clear on its own")
    ap.add_argument("--min-total", type=float, default=0.0,
                    help="the floor the whole server has to clear. It is separate from --min "
                         "because the two say different things: --min stops one package from "
                         "rotting, and this stops the suite as a whole from drifting down a "
                         "tenth at a time while every package stays just above its own floor")
    ap.add_argument("--quiet", action="store_true")
    ap.add_argument("--merged", metavar="FILE",
                    help="write the merged profile here, so `go tool cover` can read it "
                         "without counting each test binary's copy of every block")
    args = ap.parse_args()

    blocks = merge(args.profile)
    if args.merged:
        with open(args.merged, "w") as f:
            f.write("mode: atomic\n")
            for name, (n, c) in sorted(blocks.items()):
                f.write(f"{name} {n} {c}\n")

    per = by_package(blocks)

    rows = []
    for pkg, (covered, total) in per.items():
        if not total or pkg in EXCLUDED:
            continue
        rows.append((covered / total * 100, pkg, covered, total))
    rows.sort()

    under = [r for r in rows if r[0] < args.min]
    hit = sum(r[2] for r in rows)
    allof = sum(r[3] for r in rows)
    overall = hit / allof * 100 if allof else 0.0

    if not args.quiet:
        width = max(len(p) for _, p, _, _ in rows)
        for pct, pkg, covered, total in rows:
            mark = "FAIL" if pct < args.min else "ok  "
            print(f"{mark}  {pct:6.1f}%  {pkg:<{width}}  {covered}/{total}")
        mark = "FAIL" if overall < args.min_total else "    "
        print(f"\n{mark}  {overall:6.1f}%  TOTAL{' ' * (width - 5)}  {hit}/{allof}")
        if EXCLUDED:
            print("\nexcluded: " + ", ".join(sorted(EXCLUDED)))

    failed = 0
    if under:
        print(f"\n{len(under)} package(s) under {args.min:g}%:", file=sys.stderr)
        for pct, pkg, _, _ in under:
            print(f"  {pkg} at {pct:.1f}%", file=sys.stderr)
        failed = 1
    if overall < args.min_total:
        print(f"\nthe server is at {overall:.1f}%, under the {args.min_total:g}% it has to hold",
              file=sys.stderr)
        failed = 1
    return failed


if __name__ == "__main__":
    sys.exit(main())
