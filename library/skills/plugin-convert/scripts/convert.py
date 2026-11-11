#!/usr/bin/env python3
"""convert.py — copy foreign SKILL.md trees into a minimal Oswald plugin source.

Markdown only: skills transfer, runtimes do not. The source is untrusted
data: nothing here executes foreign code. Refuses symlinks, escapes, files
over 1 MiB, and skills over 128 files.

Usage:
  convert.py --source DIR --kind {opencode,hermes,claude} --out DIR
             [--name PLUGIN] [--origin-url URL --origin-rev SHA]
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
SKIP_BASENAMES = {"readme.md", "license.md", "licence.md", "changelog.md", "license"}
PLUGIN_NAME_RE = re.compile(r"^[a-z][a-z0-9_-]{1,39}$")
SKILL_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,63}$")


def fail(msg):
    print("convert: %s" % msg, file=sys.stderr)
    sys.exit(1)


def read_text(path):
    with open(path, "r", encoding="utf-8") as fh:
        return fh.read()


def split_frontmatter(text):
    """Return (keys, body) with flat column-0 key parsing (Oswald rules)."""
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
    order = []
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
        if k not in keys:
            order.append(k)
        keys[k] = v
    return {"keys": keys, "order": order}, "\n".join(lines[end + 1 :]).strip()


def hermes_env_names(text):
    """Extract required_environment_variables[].name entries (simple parse)."""
    names = []
    in_block = False
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


def check_tree(root):
    """Fail closed on symlinks, escapes, and oversize files."""
    for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
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
    """Find (skill_dir, rel_from_skills_ancestor_or_source) pairs."""
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
        # Anchor at the nearest ancestor named "skills" when present.
        if "skills" in parts:
            rel = os.sep.join(parts[parts.index("skills") + 1 :])
        else:
            # Drop leading dot-directories (.opencode, .claude, .agents).
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
        base = os.path.basename(dirpath)
        if base not in ("skills", "commands"):
            continue
        for f in sorted(filenames):
            if not f.endswith(".md") or f == "SKILL.md":
                continue
            if f.lower() in SKIP_BASENAMES:
                continue
            found.append(os.path.join(dirpath, f))
    return sorted(found)


def convert_skill(skill_dir, rel, dest_skills, dropped_fields, refused):
    name = os.path.basename(os.path.normpath(rel)).lower()
    if not SKILL_NAME_RE.match(name):
        refused.append({"skill": rel, "reason": "invalid skill name %r" % name})
        return None
    try:
        entries = []
        for dirpath, _, filenames in os.walk(skill_dir, followlinks=False):
            for f in filenames:
                entries.append(os.path.join(dirpath, f))
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
    parsed, body = split_frontmatter(text)
    if parsed is None:
        refused.append({"skill": rel, "reason": "unclosed frontmatter fence"})
        return None
    keys = parsed["keys"]
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
            dropped_fields.append(k)
    dest = os.path.join(dest_skills, rel)
    os.makedirs(dest, mode=0o755, exist_ok=True)
    front = ["---", "name: %s" % name]
    for k in ("description", "version", "required_env"):
        if k in kept:
            front.append("%s: %s" % (k, kept[k]))
    front.append("---")
    with open(os.path.join(dest, "SKILL.md"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(front) + "\n\n" + body + "\n")
    for sub in SUPPORT_DIRS:
        src_sub = os.path.join(skill_dir, sub)
        if not os.path.isdir(src_sub) or os.path.islink(src_sub):
            continue
        for dirpath, _, filenames in os.walk(src_sub, followlinks=False):
            for f in sorted(filenames):
                src_file = os.path.join(dirpath, f)
                if os.path.islink(src_file):
                    refused.append({"skill": rel, "reason": "symlink refused"})
                    return None
                if os.path.getsize(src_file) > MAX_FILE_BYTES:
                    refused.append(
                        {"skill": rel, "reason": "supporting file over 1 MiB"}
                    )
                    return None
                target = os.path.join(dest, os.path.relpath(src_file, skill_dir))
                os.makedirs(os.path.dirname(target), mode=0o755, exist_ok=True)
                with open(src_file, "rb") as fh_in:
                    data = fh_in.read()
                with open(target, "wb") as fh_out:
                    fh_out.write(data)
    return {"name": name, "source_rel": rel.replace(os.sep, "/")}


def convert_flat(md_file, source, dest_skills, refused):
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
    dest = os.path.join(dest_skills, stem)
    if os.path.exists(dest):
        refused.append({"skill": stem, "reason": "name collision with skill dir"})
        return None
    os.makedirs(dest, mode=0o755)
    with open(os.path.join(dest, "SKILL.md"), "w", encoding="utf-8") as fh:
        fh.write("---\nname: %s\n---\n\n%s\n" % (stem, text.strip()))
    return {
        "name": stem,
        "source_rel": os.path.relpath(md_file, source).replace(os.sep, "/"),
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--source", required=True)
    ap.add_argument("--kind", required=True, choices=("opencode", "hermes", "claude"))
    ap.add_argument("--out", required=True)
    ap.add_argument("--name", default="")
    ap.add_argument("--origin-url", default="")
    ap.add_argument("--origin-rev", default="")
    args = ap.parse_args()
    source = os.path.abspath(args.source)
    if not os.path.isdir(source):
        fail("source directory is unavailable")
    plugin = args.name.strip().lower() or os.path.basename(source).lower()
    if not PLUGIN_NAME_RE.match(plugin) or plugin == "soul":
        fail("invalid plugin name %r (pass --name)" % plugin)
    out = os.path.abspath(args.out)
    if os.path.exists(out):
        fail("output exists (refusing to overwrite): %s" % out)
    check_tree(source)
    dest_skills = os.path.join(out, "skills")
    os.makedirs(dest_skills, mode=0o755)
    dropped, refused, converted = [], [], []
    for skill_dir, rel in skill_leaves(source):
        got = convert_skill(skill_dir, rel, dest_skills, dropped, refused)
        if got:
            got["kind"] = "skill_dir"
            converted.append(got)
    for md_file in flat_skills(source):
        got = convert_flat(md_file, source, dest_skills, refused)
        if got:
            got["kind"] = "flat_md"
            converted.append(got)
    if not converted:
        fail("no convertible skills found")
    manifest = (
        'name: %s\nversion: "1.0"\n'
        "description: Converted %s plugin (manual conversion; see conversion.json)\n"
        'manifest_version: 2\napi_version: "1"\n'
    ) % (plugin, args.kind)
    os.makedirs(out, mode=0o755, exist_ok=True)
    with open(os.path.join(out, "plugin.yaml"), "w", encoding="utf-8") as fh:
        fh.write(manifest)
    record = {
        "kind": args.kind,
        "origin_url": args.origin_url,
        "origin_rev": args.origin_rev,
        "converted_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "plugin": plugin,
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
    if refused:
        for r in refused:
            print("convert: refused %s (%s)" % (r["skill"], r["reason"]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
