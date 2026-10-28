#!/usr/bin/env python3
"""Checks that every localized("...") key in SnagUI and SnagPostmortem is in that target's
Localizable.xcstrings, that no catalog key is unused, and that every language has every key.

    Scripts/check-strings.py            # check
    Scripts/check-strings.py --keys     # print the keys used in source, per target
"""
import json, pathlib, re, sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
TARGETS = ["SnagUI", "SnagPostmortem"]
CALL = re.compile(r'localized\("((?:[^"\\]|\\.)*)"\)')
INTERP = re.compile(r'\\\((?:[^()]|\([^()]*\))*\)')


def key_for(literal, source):
    """Swift turns interpolations into format specifiers: Int -> %lld, everything else -> %@."""
    def spec(m):
        expr = m.group(0)[2:-1]
        if re.search(r'count|unread|hidden|index|votes|\+ 1', expr):
            return "%lld"
        return "%@"
    return INTERP.sub(spec, literal).replace('\\"', '"')


def used_keys(target):
    keys = {}
    for path in sorted((ROOT / "Sources" / target).rglob("*.swift")):
        text = path.read_text()
        for m in CALL.finditer(text):
            line = text[text.rfind("\n", 0, m.start()) + 1:m.start()]
            if line.lstrip().startswith("//"):
                continue
            keys.setdefault(key_for(m.group(1), text), f"{path.name}:{text[:m.start()].count(chr(10)) + 1}")
    return keys


def main():
    failed = False
    for target in TARGETS:
        used = used_keys(target)
        if "--keys" in sys.argv:
            print(f"# {target}")
            for k in sorted(used):
                print(k)
            continue
        catalog_path = ROOT / "Sources" / target / "Resources" / "Localizable.xcstrings"
        catalog = json.loads(catalog_path.read_text())
        strings = catalog["strings"]
        languages = set()
        for entry in strings.values():
            languages |= set(entry.get("localizations", {}))
        for key, where in sorted(used.items()):
            if key not in strings:
                print(f"{target}: missing from catalog: {key!r} ({where})")
                failed = True
        for key in sorted(set(strings) - set(used)):
            print(f"{target}: unused catalog key: {key!r}")
            failed = True
        for key, entry in sorted(strings.items()):
            missing = languages - set(entry.get("localizations", {})) - {catalog["sourceLanguage"]}
            if missing and key in used:
                print(f"{target}: {key!r} has no {', '.join(sorted(missing))}")
                failed = True
        if not failed:
            print(f"{target}: {len(used)} strings, {len(languages)} languages")
    sys.exit(1 if failed else 0)


main()
