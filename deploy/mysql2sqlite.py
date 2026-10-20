#!/usr/bin/env python3
"""Turn a data-only mysqldump into SQL that SQLite loads.

The local stack runs SQLite (ADR 19), but the 103mail.com seed is a dump of the
MySQL database the PHP backend uses. Only the INSERT statements are kept, and
their string literals are rewritten: MySQL escapes with backslashes, SQLite
only doubles quotes. Backtick identifiers are left alone, SQLite accepts them.

Rows are inserted with OR IGNORE, so rows the stricter schema refuses, such as
a subscription listed twice, are skipped rather than failing the load.

Usage:  mysql2sqlite.py < dump.sql > seed.sql
"""

import sys

ESCAPES = {"0": "\0", "'": "'", '"': '"', "b": "\b", "n": "\n", "r": "\r",
           "t": "\t", "Z": "\x1a", "\\": "\\", "%": "\\%", "_": "\\_"}


def convert(statement):
    out = []
    i = 0
    while i < len(statement):
        ch = statement[i]
        if ch != "'":
            out.append(ch)
            i += 1
            continue
        # A MySQL string literal, up to its closing quote.
        i += 1
        text = []
        while i < len(statement):
            ch = statement[i]
            if ch == "\\" and i + 1 < len(statement):
                text.append(ESCAPES.get(statement[i + 1], statement[i + 1]))
                i += 2
            elif ch == "'" and statement[i + 1:i + 2] == "'":
                text.append("'")
                i += 2
            elif ch == "'":
                i += 1
                break
            else:
                text.append(ch)
                i += 1
        out.append("'" + "".join(text).replace("'", "''") + "'")
    return "".join(out)


def main():
    print("PRAGMA foreign_keys = OFF;")
    print("BEGIN;")
    statement = None
    for line in sys.stdin:
        if statement is None and line.startswith("INSERT INTO "):
            statement = line.replace("INSERT INTO ", "INSERT OR IGNORE INTO ", 1)
        elif statement is not None:
            statement += line
        if statement is not None and statement.rstrip().endswith(";"):
            sys.stdout.write(convert(statement.rstrip("\n")) + "\n")
            statement = None
    print("COMMIT;")


if __name__ == "__main__":
    main()
