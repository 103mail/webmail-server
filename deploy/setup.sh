#!/bin/bash
# Prepare everything the compose stack needs that cannot be checked in: TLS
# certificates, per-Domain federation keypairs and config, seed SQL, and the
# attachment directories.
#
# Idempotent. Re-running keeps existing certificates and keys, so the CA you
# trusted in the browser stays valid and pinned public keys do not change.
#
# Usage:  ./setup.sh [--reseed]
#   --reseed   rebuild the seed SQL even if it already exists

set -euo pipefail
cd "$(dirname "$0")"

DOMAINS=(103mail.com example.com)
CERTS=certs
CA_KEY="$CERTS/webmail-local-ca.key"
CA_CRT="$CERTS/webmail-local-ca.crt"
RESEED=false
[[ "${1:-}" == "--reseed" ]] && RESEED=true

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

# ── Attachment directories ──────────────────────────────────────────────────
mkdir -p data/103mail.com/attachments data/103mail.com/documents \
         data/example.com/attachments data/example.com/documents

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
# Data only. The schema comes from ../schema/webmail.sql, mounted alongside.
if [[ ! -s seed/103mail.com.sql ]] || $RESEED; then
    echo "dumping the local webmail database into seed/103mail.com.sql"
    if ! mysqldump -u webmail -pwebmail --no-create-info --skip-extended-insert \
            --single-transaction --no-tablespaces webmail > seed/103mail.com.sql 2>/dev/null; then
        echo "  no local webmail database reachable; writing a minimal seed instead" >&2
        ./seed.sh 103mail.com "maurits@vdschee.nl" "Maurits van der Schee" \
            "mevdschee@103mail.com" > seed/103mail.com.sql
    else
        # The dump has no domain row for the Domain this Server hosts. Design 3
        # retires the "customer_id IS NULL" singleton, so mark it local instead.
        cat >> seed/103mail.com.sql <<'SQL'

-- The Domain this Server is authoritative for.
INSERT INTO `domain` (`name`, `customer_id`) SELECT '103mail.com', 1
  WHERE NOT EXISTS (SELECT 1 FROM `domain` WHERE `name` = '103mail.com');
SQL
    fi
fi

if [[ ! -s seed/example.com.sql ]] || $RESEED; then
    echo "writing seed/example.com.sql"
    ./seed.sh example.com "bob@example.com" "Bob Jansen" "bob@example.com" \
        > seed/example.com.sql
fi

echo
echo "done. Next:"
echo "  1. add to /etc/hosts:"
echo "       127.0.0.1  webmail.103mail.com"
echo "       127.0.0.2  webmail.example.com"
echo "  2. trust $PWD/$CA_CRT in your browser"
echo "  3. docker compose up -d --build"
