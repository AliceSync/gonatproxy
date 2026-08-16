#!/usr/bin/env bash
# Generates a self-signed CA and a server certificate signed by it.
# Run on the SERVER host. Copy ca.crt (and nothing else) to client hosts.
#
#   usage: ./gen_certs.sh [SAN_IP1 SAN_IP2 ... SAN_HOSTNAME]
#   e.g.: ./gen_certs.sh 10.0.0.5 ai.example.com
set -euo pipefail

cd "$(dirname "$0")/.."
OUT=certs
mkdir -p "$OUT"
cd "$OUT"

# Build SAN list. If none given, default to the server's own hostname and
# loopback addresses.
SAN="DNS:localhost,IP:127.0.0.1"
for arg in "$@"; do
  if [[ "$arg" =~ ^[0-9.]+$ ]] || [[ "$arg" =~ ^[0-9a-fA-F:]+$ ]]; then
    SAN="$SAN,IP:$arg"
  else
    SAN="$SAN,DNS:$arg"
  fi
done

cat > openssl.cnf <<EOF
[req]
distinguished_name=dn
[dn]
[ext]
subjectAltName=$SAN
EOF

# CA key + cert
openssl genrsa -out ca.key 4096 2>/dev/null
openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 \
  -subj "/CN=deepseek-ai-tunnel-ca" -out ca.crt

# Server key + CSR + cert signed by CA
openssl genrsa -out server.key 2048 2>/dev/null
openssl req -new -key server.key -subj "/CN=deepseek-ai-tunnel-server" -out server.csr
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt -days 3650 -sha256 -extfile openssl.cnf -extensions ext

chmod 600 ca.key server.key
rm -f server.csr
echo "done -> certs/: ca.crt ca.key server.crt server.key"
echo "copy certs/ca.crt to the client and point ca_file at it"
