#!/bin/bash
# Prepare everything the compose stack needs that cannot be checked in: TLS
# certificates, per-Domain federation keypairs and config, seed SQL, the
# attachment directories, and one SQLite database per Domain (ADR 19).
#
# Idempotent. Re-running keeps existing certificates and keys, so the CA you
# trusted in the browser stays valid and pinned public keys do not change.
#
# Usage:  ./setup.sh [--reseed]
#   --reseed   rebuild the seed SQL and the databases even if they exist,
#              discarding whatever the stack has written to them

set -euo pipefail
cd "$(dirname "$0")"

DOMAINS=(103mail.com example.com mail.acme.test acme.test)
CERTS=certs
CA_KEY="$CERTS/webmail-local-ca.key"
CA_CRT="$CERTS/webmail-local-ca.crt"
RESEED=false
[[ "${1:-}" == "--reseed" ]] && RESEED=true

if ! command -v sqlite3 >/dev/null; then
    echo "sqlite3 is needed to build the databases (apt install sqlite3)" >&2
    exit 1
fi

mkdir -p "$CERTS" seed keys

# ── A local CA, so one certificate has to be trusted rather than two ─────────
if [[ ! -f "$CA_CRT" ]]; then
    echo "creating local CA"
    openssl req -x509 -newkey rsa:4096 -sha256 -days 3650 -nodes \
        -keyout "$CA_KEY" -out "$CA_CRT" \
        -subj "/CN=Webmail local development CA/O=webmail-server deploy" \
        -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
        -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
fi

# ── Per-Domain leaf certificates for the Federation hostname ────────────────
# The RFC requires a Web PKI certificate valid for webmail.{domain} (RFC 9.2).
# These are not that, and a peer implementing the RFC would refuse them. They
# exist so the browser can reach the local stack over the real URLs.
for d in "${DOMAINS[@]}"; do
    host="webmail.$d"
    if [[ -f "$CERTS/$host.crt" ]]; then continue; fi
    echo "creating certificate for $host"
    openssl req -newkey rsa:2048 -nodes \
        -keyout "$CERTS/$host.key" -out "$CERTS/$host.csr" \
        -subj "/CN=$host" 2>/dev/null
    openssl x509 -req -in "$CERTS/$host.csr" -days 825 -sha256 \
        -CA "$CA_CRT" -CAkey "$CA_KEY" -CAcreateserial \
        -extfile <(printf 'subjectAltName=DNS:%s\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' "$host") \
        -out "$CERTS/$host.crt" 2>/dev/null
    rm -f "$CERTS/$host.csr"
done
chmod 644 "$CERTS"/*.key "$CERTS"/*.crt

# ── Per-Domain Ed25519 federation keypairs ──────────────────────────────────
# Each Domain gets its own key; RFC 4.1 forbids sharing one across co-hosted
# Domains, because peers pin keys per DNS domain. The peer descriptor is
# rendered by the server from this key rather than written here, so the
# published key and the signing key cannot drift apart.
for d in "${DOMAINS[@]}"; do
    key="keys/$d.ed25519.pem"
    if [[ ! -f "$key" ]]; then
        echo "creating federation keypair for $d"
        openssl genpkey -algorithm ed25519 -out "$key" 2>/dev/null
        chmod 644 "$key"   # read by the container's unprivileged user
    fi
    mkdir -p "domain/$d"
    if [[ ! -f "domain/$d/federation.json" ]]; then
        echo "writing domain/$d/federation.json"
        cat > "domain/$d/federation.json" <<JSON
{
  "domains": [
    {
      "domain": "$d",
      "customerId": 1,
      "privateKeyFile": "/etc/webmail/keys/$d.ed25519.pem"
    }
  ],
  "allowedPeers": [],
  "maxEnvelopeBytes": 10485760,
  "maxAttachmentBytes": 26214400,
  "extraCaFile": "/etc/webmail/certs/webmail-local-ca.crt"
}
JSON
    fi
done

# ── The shared clock offset ─────────────────────────────────────────────────
# Every Domain's API and worker read it (WEBMAIL_CLOCK_FILE). The end-to-end
# suite moves it forward to let time pass without waiting.
mkdir -p clock
[[ -s clock/offset ]] || echo 0s > clock/offset

# ── Attachment directories ──────────────────────────────────────────────────
for d in "${DOMAINS[@]}"; do
    mkdir -p "data/$d/attachments" "data/$d/documents"
done

# 103mail.com is seeded from the local development database, so it needs that
# database's attachment bytes to go with it. Copied rather than bind-mounted:
# the stack writes new attachments, and it should not write into the directory
# the PHP backend reads.
PHP_DATA=../../webmail/data/attachments
if [[ -d "$PHP_DATA" ]] && [[ -z "$(ls -A data/103mail.com/attachments 2>/dev/null)" ]]; then
    echo "copying attachment bytes from $PHP_DATA"
    cp -r "$PHP_DATA/." data/103mail.com/attachments/
fi

# ── Seed SQL ────────────────────────────────────────────────────────────────
# Data only, as SQLite SQL. The schema comes from ../schema/webmail.sqlite.sql.
if [[ ! -s seed/103mail.com.sqlite.sql ]] || $RESEED; then
    echo "dumping the local webmail database into seed/103mail.com.mysql.sql"
    if mysqldump -u webmail -pwebmail --no-create-info --skip-extended-insert \
            --complete-insert --single-transaction --no-tablespaces webmail \
            > seed/103mail.com.mysql.sql 2>/dev/null; then
        dump=seed/103mail.com.mysql.sql
    elif [[ -s seed/103mail.com.sql ]] && grep -q '^INSERT INTO' seed/103mail.com.sql; then
        # A dump left by an earlier version of this script, from when the
        # stack ran MariaDB.
        echo "  no local webmail database reachable; converting the earlier dump in seed/103mail.com.sql"
        dump=seed/103mail.com.sql
    else
        dump=""
    fi
    if [[ -n "$dump" ]]; then
        # The dump has no domain row for the Domain this Server hosts. Design
        # 3 retires the "customer_id IS NULL" singleton, so mark it local.
        {
            cat "$dump"
            printf '%s\n' "INSERT INTO \`domain\` (\`name\`, \`customer_id\`) SELECT '103mail.com', 1" \
                "  WHERE NOT EXISTS (SELECT 1 FROM \`domain\` WHERE \`name\` = '103mail.com');"
        } | ./mysql2sqlite.py > seed/103mail.com.sqlite.sql
    else
        echo "  no local webmail database reachable; writing a minimal seed instead" >&2
        ./seed.sh 103mail.com "maurits@vdschee.nl" "Maurits van der Schee" \
            "mevdschee@103mail.com" > seed/103mail.com.sqlite.sql
    fi
fi

if [[ ! -s seed/example.com.sqlite.sql ]] || $RESEED; then
    echo "writing seed/example.com.sqlite.sql"
    ./seed.sh example.com "bob@example.com" "Bob Jansen" "bob@example.com" \
        > seed/example.com.sqlite.sql
fi

# A Domain on a subdomain, which the end-to-end stories need (RFC 4.1.1). Its
# Federation hostname is webmail.mail.acme.test, and it is a Domain of its own,
# unrelated to acme.test.
if [[ ! -s seed/mail.acme.test.sqlite.sql ]] || $RESEED; then
    echo "writing seed/mail.acme.test.sqlite.sql"
    ./seed.sh mail.acme.test "erin@mail.acme.test" "Erin Bakker" "erin@mail.acme.test" \
        > seed/mail.acme.test.sqlite.sql
fi

# The parent of mail.acme.test, and a Domain of its own: a parent and its
# subdomain share nothing on the wire (RFC 4.1.1).
if [[ ! -s seed/acme.test.sqlite.sql ]] || $RESEED; then
    echo "writing seed/acme.test.sqlite.sql"
    ./seed.sh acme.test "pete@acme.test" "Pete Smit" "pete@acme.test" \
        > seed/acme.test.sqlite.sql
fi

# ── Databases ───────────────────────────────────────────────────────────────
# One SQLite file per Domain, in the directory that Domain's API and worker
# both mount as /data. Built once from the schema and the seed, with every
# address lowercased the way the server stores them. Delete a file, or pass
# --reseed, to start that Domain over.
#
# The schema has to apply cleanly. The seed does not: a dump of a database
# whose schema has drifted from ours loses the statements that do not fit,
# which are listed, rather than the whole Domain.
for d in "${DOMAINS[@]}"; do
    db="data/$d/webmail.db"
    if [[ -f "$db" ]] && ! $RESEED; then continue; fi
    echo "building $db"
    rm -f "$db" "$db-wal" "$db-shm"
    cat ../schema/webmail.sqlite.sql ../schema/search.sqlite.sql | sqlite3 -bail "$db"
    errors=$(cat "seed/$d.sqlite.sql" lowercase.sql | sqlite3 "$db" 2>&1 >/dev/null || true)
    if [[ -n "$errors" ]]; then
        echo "  $(grep -c . <<<"$errors") seed statements failed and were skipped, the first:" >&2
        head -n 3 <<<"$errors" | sed 's/^/    /' >&2
    fi
done

echo
echo "done. Next:"
echo "  1. add to /etc/hosts:"
echo "       127.0.0.1  webmail.103mail.com"
echo "       127.0.0.2  webmail.example.com"
echo "       127.0.0.3  webmail.mail.acme.test"
echo "       127.0.0.4  webmail.acme.test"
echo "  2. trust $PWD/$CA_CRT in your browser"
echo "  3. docker compose up -d --build"
