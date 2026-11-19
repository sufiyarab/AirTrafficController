#!/bin/sh
# Dev certificates for mutual TLS (internal/mtls): one CA, and one
# certificate per service with the service name as its subject alternative
# name. Compose runs this once in the one-shot "certs" service before anything
# else starts; it does nothing if the CA already exists.
#
#   scripts/gen-certs.sh [DIR] [NAME ...]
#
# DIR defaults to ./certs (git-ignored). NAMEs default to every service in
# docker-compose.yml. Delete DIR to start over with a new CA.
#
# Demo only: the private keys are world-readable so every container (and you,
# for the curl demo) can read them. Never reuse these anywhere else.
set -eu

DIR=${1:-./certs}
[ $# -gt 0 ] && shift
if [ $# -eq 0 ]; then
  set -- gateway simulator receiver-a receiver-b receiver-c
  for c in sector-1 sector-2 sector-3 sector-4 tower registry standby; do
    set -- "$@" "$c-a" "$c-b" "$c-c"
  done
fi

mkdir -p "$DIR"
if [ -f "$DIR/ca.crt" ]; then
  echo "gen-certs: $DIR/ca.crt exists, keeping the current certificates"
  exit 0
fi

DAYS=365
openssl ecparam -name prime256v1 -genkey -noout -out "$DIR/ca.key"
openssl req -x509 -new -key "$DIR/ca.key" -days $DAYS -subj "/CN=ATC dev CA" -out "$DIR/ca.crt"

for name in "$@"; do
  # An IP address (local runs without Docker DNS) gets an IP SAN, a name a DNS SAN.
  case "$name" in
    *[!0-9.]*) san="DNS:$name" ;;
    *) san="IP:$name" ;;
  esac
  ext=$(mktemp)
  printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth,clientAuth\nkeyUsage=digitalSignature\n' "$san" > "$ext"
  openssl ecparam -name prime256v1 -genkey -noout -out "$DIR/$name.key"
  openssl req -new -key "$DIR/$name.key" -subj "/CN=$name" -out "$DIR/$name.csr"
  openssl x509 -req -in "$DIR/$name.csr" -CA "$DIR/ca.crt" -CAkey "$DIR/ca.key" -CAcreateserial \
    -days $DAYS -extfile "$ext" -out "$DIR/$name.crt" 2>/dev/null
  rm -f "$DIR/$name.csr" "$ext"
done
chmod 644 "$DIR"/*.key
rm -f "$DIR/ca.srl"
echo "gen-certs: CA and $# certificates in $DIR"
