#!/bin/bash
# Render webmail.dbml into the DDL each engine runs (ADR 19). The output is
# checked in, so the server and the tests need no dbml-tools to build.
#
# Usage:  ./generate.sh [path/to/dbml-tools]
#
# Needs dbml-tools 0.9.10 or later: earlier releases emit no indexes, and the
# ON CONFLICT clauses in the code depend on the composite unique indexes.

set -euo pipefail
cd "$(dirname "$0")"

DBML_TOOLS=${1:-dbml-tools}
MIN_VERSION=0.9.10

version=$("$DBML_TOOLS" version | awk 'NR == 1 { print $2 }')
if [[ "$(printf '%s\n%s\n' "$MIN_VERSION" "$version" | sort -V | head -n 1)" != "$MIN_VERSION" ]]; then
    echo "$DBML_TOOLS is version $version; generate.sh needs $MIN_VERSION or later" >&2
    exit 1
fi

"$DBML_TOOLS" check webmail.dbml

{
    echo "-- Generated from webmail.dbml by generate.sh. Edit that, not this."
    echo
    "$DBML_TOOLS" tosql webmail.dbml
} > webmail.postgresql.sql

# The DBML names PostgreSQL as its database_type; SQLite gets the same model
# with only that setting changed.
tmp=$(mktemp --suffix=.dbml)
trap 'rm -f "$tmp"' EXIT
sed "s/database_type: 'PostgreSQL'/database_type: 'SQLite'/" webmail.dbml > "$tmp"
{
    echo "-- Generated from webmail.dbml by generate.sh. Edit that, not this."
    echo
    "$DBML_TOOLS" tosql "$tmp"
} > webmail.sqlite.sql

grep -q 'CREATE UNIQUE INDEX' webmail.sqlite.sql || {
    echo "$DBML_TOOLS emitted no indexes" >&2
    exit 1
}
