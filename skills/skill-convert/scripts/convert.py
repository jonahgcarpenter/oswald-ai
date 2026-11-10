#!/usr/bin/env python3
"""convert.py — copy foreign SKILL.md trees into a minimal Oswald skill source.

Markdown only: frontmatter is trimmed to name/description/version/required_env
(an exact Hermes required_environment_variables[].name mapping included) and
everything else is recorded as dropped. The source is untrusted data: nothing
here executes foreign code. Refuses symlinks, escapes, files over 1 MiB, and
skills over 128 files.

Usage:
  convert.py --source DIR --out DIR [--name SKILL]
             [--origin-url URL --origin-rev SHA]
"""

import argparse
import json
import os
import re
import sys
from datetime import datetime, timezone

MAX_FILE_BYTES = 1 << 20
MAX_SKILL_FILES = 128
SUPPORT_DIRS = ("scripts", "references", "templates", "assets")
SKIP_BASENAMES = ("readme.md", "license.md", "licence.md", "changelog.md")
SKILL_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,63}$")


def fail(msg):
    print("convert: %s" % msg, file=sys.stderr)
    sys.exit(1)


def read_text(path):
    with open(path, "r", encoding="utf-8") as fh:
        return fh.read()


def split_frontmatter(text):
    """Flat column-0 key parse (Oswald rules); unclosed fence -> (None, '')."""
    lines = text.split("\n")
    if not lines or lines[0].strip() != "---":
        return {}, text.strip()
    end = -1
    for i in range(1, len(lines)):
        if lines[i].strip() == "---":
            end = i
            break
    if end < 0:
        return None, ""
    keys = {}
    for line in lines[1:end]:
        if not line or line[0] in (" ", "\t"):
            continue
        t = line.strip()
        if not t or t.startswith("#") or ":" not in t:
            continue
        k, v = t.split(":", 1)
        k = k.strip().lower()
        v = v.strip()
        if len(v) >= 2 and v[0] == v[-1] and v[0] in ("'", '"'):
            v = v[1:-1]
        keys[k] = v
    return keys, "\n".join(lines[end + 1 :]).strip()


def hermes_env_names(text):
    """required_environment_variables[].name entries (simple parse)."""
    names, in_block = [], False
    for line in text.split("\n"):
        if not line or line[0] in (" ", "\t"):
            if in_block:
                m = re.match(r"\s*-\s*name\s*:\s*(\S+)", line)
                if m:
                    names.append(m.group(1).strip().strip("'\""))
            continue
        t = line.strip()
        if not t or t.startswith("#"):
            if in_block and ":" in t:
                in_block = False
            continue
        key = t.split(":", 1)[0].strip().lower()
        in_block = key == "required_environment_variables"
    seen = []
    for n in names:
        if re.match(r"^[A-Za-z_][A-Za-z0-9_]*$", n) and n not in seen:
            seen.append(n)
    return seen


def check_tree(source):
    for dirpath, dirnames, filenames in os.walk(source, followlinks=False):
        for d in dirnames:
            if os.path.islink(os.path.join(dirpath, d)):
                fail("symlink refused: %s" % os.path.join(dirpath, d))
        for f in filenames:
            p = os.path.join(dirpath, f)
            if os.path.islink(p):
                fail("symlink refused: %s" % p)
            if os.path.getsize(p) > MAX_FILE_BYTES:
                fail("file over 1 MiB refused: %s" % p)


def skill_leaves(source):
    """Dirs holding SKILL.md (no descent into a found skill), with rel paths
    anchored at the nearest ancestor 'skills' dir, else repo-root relative
    with leading dot-directories stripped."""
    if os.path.isfile(os.path.join(source, "SKILL.md")):
        return [(source, ".")]
    leaves = []
    for dirpath, dirnames, filenames in os.walk(source, followlinks=False):
        dirnames.sort()
        if "SKILL.md" in filenames:
            leaves.append(dirpath)
            dirnames[:] = []
    out = []
    for leaf in sorted(leaves):
        rel = os.path.relpath(leaf, source)
        parts = rel.split(os.sep)
        if "skills" in parts:
            rel = os.sep.join(parts[parts.index("skills") + 1 :])
        else:
            while parts and parts[0].startswith("."):
                parts = parts[1:]
            rel = os.sep.join(parts) if parts else "."
        if rel in (".", ""):
            rel = os.path.basename(os.path.normpath(leaf))
        out.append((leaf, rel))
    return out


def flat_skills(source):
    """Convertible bare *.md files: direct children of skills|commands dirs."""
    found = []
    for dirpath, _, filenames in os.walk(source, followlinks=False):
        if os.path.basename(dirpath) not in ("skills", "commands"):
            continue
        for f in sorted(filenames):
            if not f.endswith(".md") or f == "SKILL.md":
                continue
            if f.lower() in SKIP_BASENAMES:
                continue
            found.append(os.path.join(dirpath, f))
    return sorted(found)


def write_skill_md(dest, name, kept, body):
    lines = ["---", "name: %s" % name]
    for k in ("description", "version", "required_env"):
        if k in kept:
            lines.append("%s: %s" % (k, kept[k]))
    lines.append("---")
    with open(os.path.join(dest, "SKILL.md"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n\n" + body + "\n")


def copy_supports(skill_dir, dest, refused, rel):
    for sub in SUPPORT_DIRS:
        src_sub = os.path.join(skill_dir, sub)
        if not os.path.isdir(src_sub) or os.path.islink(src_sub):
            continue
        for dirpath, _, filenames in os.walk(src_sub, followlinks=False):
            for f in sorted(filenames):
                src_file = os.path.join(dirpath, f)
                if os.path.islink(src_file):
                    refused.append({"skill": rel, "reason": "symlink refused"})
                    return False
                if os.path.getsize(src_file) > MAX_FILE_BYTES:
                    refused.append(
                        {"skill": rel, "reason": "supporting file over 1 MiB"}
                    )
                    return False
                target = os.path.join(dest, os.path.relpath(src_file, skill_dir))
                os.makedirs(os.path.dirname(target), mode=0o755, exist_ok=True)
                with open(src_file, "rb") as fh_in:
                    data = fh_in.read()
                with open(target, "wb") as fh_out:
                    fh_out.write(data)
    return True


def convert_skill(skill_dir, rel, dest_root, dropped, refused):
    name = os.path.basename(os.path.normpath(rel)).lower()
    if not SKILL_NAME_RE.match(name):
        refused.append({"skill": rel, "reason": "invalid skill name %r" % name})
        return None
    try:
        entries = []
        for dirpath, _, filenames in os.walk(skill_dir, followlinks=False):
            entries.extend(filenames)
    except OSError as exc:
        refused.append({"skill": rel, "reason": "unreadable: %s" % exc})
        return None
    if len(entries) > MAX_SKILL_FILES:
        refused.append({"skill": rel, "reason": "over 128 files"})
        return None
    try:
        text = read_text(os.path.join(skill_dir, "SKILL.md"))
    except OSError as exc:
        refused.append({"skill": rel, "reason": "unreadable SKILL.md: %s" % exc})
        return None
    if len(text.encode("utf-8")) > MAX_FILE_BYTES:
        refused.append({"skill": rel, "reason": "SKILL.md over 1 MiB"})
        return None
    keys, body = split_frontmatter(text)
    if keys is None:
        refused.append({"skill": rel, "reason": "unclosed frontmatter fence"})
        return None
    kept = {}
    if keys.get("description"):
        kept["description"] = keys["description"]
    if keys.get("version"):
        kept["version"] = keys["version"]
    env = []
    if keys.get("required_env"):
        for part in keys["required_env"].strip("[]").split(","):
            p = part.strip()
            if re.match(r"^[A-Za-z_][A-Za-z0-9_]*$", p) and p not in env:
                env.append(p)
    for n in hermes_env_names(text):
        if n not in env:
            env.append(n)
    if env:
        kept["required_env"] = "[%s]" % ", ".join(env)
    for k in keys:
        if k not in (
            "name",
            "description",
            "version",
            "required_env",
            "required_environment_variables",
        ):
            dropped.append(k)
    dest = os.path.join(dest_root, rel)
    if os.path.exists(dest):
        refused.append({"skill": rel, "reason": "name collision"})
        return None
    os.makedirs(dest, mode=0o755)
    write_skill_md(dest, name, kept, body)
    if not copy_supports(skill_dir, dest, refused, rel):
        return None
    return {"name": name, "source_rel": rel.replace(os.sep, "/"), "kind": "skill_dir"}


def convert_flat(md_file, source, dest_root, refused):
    stem = os.path.splitext(os.path.basename(md_file))[0].lower()
    if not SKILL_NAME_RE.match(stem):
        refused.append({"skill": md_file, "reason": "invalid skill name"})
        return None
    try:
        if os.path.getsize(md_file) > MAX_FILE_BYTES:
            refused.append({"skill": md_file, "reason": "file over 1 MiB"})
            return None
        text = read_text(md_file)
    except OSError as exc:
        refused.append({"skill": md_file, "reason": "unreadable: %s" % exc})
        return None
    dest = os.path.join(dest_root, stem)
    if os.path.exists(dest):
        refused.append({"skill": stem, "reason": "name collision"})
        return None
    os.makedirs(dest, mode=0o755)
    keys, body = split_frontmatter(text)
    if keys is None:
        body = text.strip()
        keys = {}
    kept = {}
    if keys.get("description"):
        kept["description"] = keys["description"]
    with open(os.path.join(dest, "SKILL.md"), "w", encoding="utf-8") as fh:
        lines = ["---", "name: %s" % stem]
        if "description" in kept:
            lines.append("description: %s" % kept["description"])
        lines.append("---")
        fh.write("\n".join(lines) + "\n\n" + body.strip() + "\n")
    return {
        "name": stem,
        "source_rel": os.path.relpath(md_file, source).replace(os.sep, "/"),
        "kind": "flat_md",
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--source", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--name", default="")
    ap.add_argument("--origin-url", default="")
    ap.add_argument("--origin-rev", default="")
    args = ap.parse_args()
    source = os.path.abspath(args.source)
    if not os.path.isdir(source):
        fail("source directory is unavailable")
    out = os.path.abspath(args.out)
    if os.path.exists(out):
        fail("output exists (refusing to overwrite): %s" % out)
    check_tree(source)
    os.makedirs(out, mode=0o755)
    if args.name.strip():
        hint = args.name.strip().lower()
        if not SKILL_NAME_RE.match(hint):
            fail("invalid --name %r" % args.name)
    dropped, refused, converted = [], [], []
    for skill_dir, rel in skill_leaves(source):
        if args.name.strip():
            rel = args.name.strip().lower()
        got = convert_skill(skill_dir, rel, out, dropped, refused)
        if got:
            converted.append(got)
            if args.name.strip():
                break
    if not args.name.strip():
        for md_file in flat_skills(source):
            got = convert_flat(md_file, source, out, refused)
            if got:
                converted.append(got)
    if not converted:
        fail("no convertible skills found")
    record = {
        "origin_url": args.origin_url,
        "origin_rev": args.origin_rev,
        "converted_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "skills": converted,
        "dropped_fields": sorted(set(dropped)),
        "refused": refused,
    }
    with open(os.path.join(out, "conversion.json"), "w", encoding="utf-8") as fh:
        json.dump(record, fh, indent=2)
        fh.write("\n")
    print(
        "convert: ok (%d skills, %d refused) -> %s"
        % (len(converted), len(refused), out)
    )
    for r in refused:
        print("convert: refused %s (%s)" % (r["skill"], r["reason"]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
