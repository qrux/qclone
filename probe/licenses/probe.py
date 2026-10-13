#!/usr/bin/env python3
"""License probe for qclone: what comparable tools use, and what qclone carries.

  1. Homebrew core: the license of every formula, and of the Go-built ones
     (build dependency "go"), from Homebrew's public API.
  2. qclone's own non-standard-library Go dependencies (go list).
  3. Each candidate license's length in words, from the SPDX license list.

Writes probe/licenses/out/results.json; prints a summary with no paths.
"""
import collections, json, os, subprocess, sys, urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "out")
REPO = os.path.dirname(os.path.dirname(HERE))
CANDIDATES = ["MIT", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "ISC", "MPL-2.0",
              "GPL-3.0-only", "AGPL-3.0-only", "Unlicense", "0BSD"]


def fetch(url):
    req = urllib.request.Request(url, headers={"User-Agent": "qclone-license-probe"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return r.read()


def family(lic):
    """Fold an SPDX expression to a family: GPL-3.0-or-later -> GPL-3.0."""
    if lic is None:
        return "(none)"
    if not isinstance(lic, str):
        return "(compound)"  # any_of / all_of objects
    for p, f in (("GPL-3.0", "GPL-3.0"), ("GPL-2.0", "GPL-2.0"), ("LGPL", "LGPL"), ("AGPL", "AGPL-3.0")):
        if lic.startswith(p):
            return f
    return lic


def main():
    os.makedirs(OUT, exist_ok=True)
    res = {}

    formulae = json.loads(fetch("https://formulae.brew.sh/api/formula.json"))
    def tally(fs):
        c = collections.Counter(family(f.get("license")) for f in fs)
        n = len(fs)
        return n, [(k, v, round(100 * v / n, 1)) for k, v in c.most_common(12)]
    go = [f for f in formulae if "go" in (f.get("build_dependencies") or [])]
    res["homebrew_all"] = tally(formulae)
    res["homebrew_go"] = tally(go)

    deps = subprocess.run(["go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./..."],
                          cwd=REPO, capture_output=True, text=True, check=True).stdout.split()
    res["qclone_third_party"] = sorted(d for d in deps if not d.startswith("qclone"))

    words = {}
    for lic in CANDIDATES:
        try:
            words[lic] = len(fetch(f"https://raw.githubusercontent.com/spdx/license-list-data/main/text/{lic}.txt").decode().split())
        except Exception as e:
            words[lic] = None
    res["license_words"] = words

    with open(os.path.join(OUT, "results.json"), "w") as f:
        json.dump(res, f, indent=1)

    for key, label in (("homebrew_all", "all Homebrew core formulae"), ("homebrew_go", "Homebrew core formulae built with Go")):
        n, rows = res[key]
        print(f"{label}: {n}")
        for k, v, pct in rows:
            print(f"  {k:<16} {v:>5}  {pct:>5}%")
    print("qclone third-party Go modules:", res["qclone_third_party"] or "none (standard library only)")
    print("license length, words:")
    for k, v in words.items():
        print(f"  {k:<16} {v}")


if __name__ == "__main__":
    sys.exit(main())
