#!/bin/sh
# Fails if any tracked file is stored in the git index with CRLF or mixed line endings.
# The index is checked rather than the working tree so the result is identical on
# Windows and Linux hosts regardless of core.autocrlf.
set -eu

bad="$(git ls-files --eol | awk '$1 == "i/crlf" || $1 == "i/mixed" { print $NF }')"

if [ -n "$bad" ]; then
	echo "Files stored with CRLF or mixed line endings:"
	echo "$bad"
	echo "Fix with: git add --renormalize <file>"
	exit 1
fi

echo "line-endings: ok"
