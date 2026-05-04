#!/usr/bin/env bash
#
# dirdiff.sh — compare two directory trees and report files that differ
# or are absent between them.
#
# Usage:
#   dirdiff.sh -src <dir> -dest <dir> [-ignore <name>] [-ignore <name> ...]
#
# Output (one line per file):
#   DIFF   <relative/path>      file exists in both but contents differ
#   ONLY_SRC   <relative/path>  file present only in -src
#   ONLY_DEST  <relative/path>  file present only in -dest
#
# With -showdiff, each DIFF line is followed by a unified diff of the two files.

set -u

usage() {
    cat <<EOF
Usage: $(basename "$0") -src <dir> -dest <dir> [-ignore <name>]... [-showdiff]

  -src <dir>      Source directory tree.
  -dest <dir>     Destination directory tree.
  -ignore <name>  Directory (or file) basename to skip. Repeat for multiple.
                  Matches any path component with that name.
  -showdiff       After each DIFF line, print a unified diff of the two files.

Examples:
  $(basename "$0") -src ./a -dest ./b
  $(basename "$0") -src ./a -dest ./b -ignore .git -ignore node_modules
  $(basename "$0") -src ./a -dest ./b -showdiff
EOF
}

SRC=""
DEST=""
IGNORES=(.DS_Store)
SHOWDIFF=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        -src)      SRC="${2:-}"; shift 2 ;;
        -dest)     DEST="${2:-}"; shift 2 ;;
        -ignore)   IGNORES+=("${2:-}"); shift 2 ;;
        -showdiff) SHOWDIFF=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
done

if [[ -z "$SRC" || -z "$DEST" ]]; then
    echo "error: -src and -dest are required" >&2
    usage >&2
    exit 2
fi

if [[ ! -d "$SRC" ]]; then
    echo "error: src is not a directory: $SRC" >&2
    exit 2
fi
if [[ ! -d "$DEST" ]]; then
    echo "error: dest is not a directory: $DEST" >&2
    exit 2
fi

# Build a path filter: returns 0 (skip) if any path component matches an ignore.
should_ignore() {
    local rel="$1"
    local part
    if [[ ${#IGNORES[@]} -eq 0 ]]; then
        return 1
    fi
    IFS='/' read -ra parts <<< "$rel"
    for part in "${parts[@]}"; do
        for ig in "${IGNORES[@]}"; do
            [[ "$part" == "$ig" ]] && return 0
        done
    done
    return 1
}

# List relative file paths under a root, NUL-separated, ignores applied.
list_files() {
    local root="$1"
    # -print0 to be safe with weird names
    (cd "$root" && find . -type f -print0) | \
    while IFS= read -r -d '' f; do
        # strip leading ./
        local rel="${f#./}"
        if ! should_ignore "$rel"; then
            printf '%s\0' "$rel"
        fi
    done
}

# Read NUL-separated list into a sorted, NL-separated tmp file.
tmp_src=$(mktemp)
tmp_dest=$(mktemp)
trap 'rm -f "$tmp_src" "$tmp_dest"' EXIT

list_files "$SRC"  | tr '\0' '\n' | LC_ALL=C sort > "$tmp_src"
list_files "$DEST" | tr '\0' '\n' | LC_ALL=C sort > "$tmp_dest"

# Files only in src
LC_ALL=C comm -23 "$tmp_src" "$tmp_dest" | while IFS= read -r rel; do
    [[ -z "$rel" ]] && continue
    printf 'ONLY_SRC   %s\n' "$rel"
done

# Files only in dest
LC_ALL=C comm -13 "$tmp_src" "$tmp_dest" | while IFS= read -r rel; do
    [[ -z "$rel" ]] && continue
    printf 'ONLY_DEST  %s\n' "$rel"
done

# Files in both: byte-compare
LC_ALL=C comm -12 "$tmp_src" "$tmp_dest" | while IFS= read -r rel; do
    [[ -z "$rel" ]] && continue
    if ! cmp -s -- "$SRC/$rel" "$DEST/$rel"; then
        printf 'DIFF       %s\n' "$rel"
        if [[ "$SHOWDIFF" -eq 1 ]]; then
            diff -u --label "src/$rel" --label "dest/$rel" \
                -- "$SRC/$rel" "$DEST/$rel" || true
        fi
    fi
done
