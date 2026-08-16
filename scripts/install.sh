#!/usr/bin/env bash
# Builds + installs binaries, configs and systemd units for a given role.
#   sudo ./scripts/install.sh server
#   sudo ./scripts/install.sh client [USER]
#   sudo ./scripts/install.sh natclient CLIENT_ID [USER]
# For manual (non-systemd) running, see CLI section of the README:
#   deepseek-server [run|start] -config .../server.json
set -euo pipefail

ROLE="${1:-}"
ARG2="${2:-}"
ARG3="${3:-}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST=/usr/local/bin
CONF=/etc/deepseek
SYSD=/etc/systemd/system

case "$ROLE" in
  server|client|natclient) ;;
  *) echo "usage: $0 server|client|natclient [CLIENT_ID|USER] [USER]" >&2; exit 1 ;;
esac

# build binaries (all three)
"$ROOT/scripts/build.sh" >/dev/null
install -m 0755 -o root -g root \
  "$ROOT/bin/deepseek-server" "$ROOT/bin/deepseek-client" "$ROOT/bin/deepseek-natclient" "$DEST"
mkdir -p "$CONF"

case "$ROLE" in
  server)
    if [[ ! -f "$CONF/server.json" ]]; then
      install -m 0644 "$ROOT/configs/server.json" "$CONF/server.json"
    fi
    # certs: use ACME-managed, or generate self-signed here
    if [[ ! -f "$CONF/certs/server.crt" || ! -f "$CONF/certs/server.key" ]]; then
      mkdir -p "$CONF/certs"
      ( cd "$ROOT" && ./scripts/gen_certs.sh && cp -r certs/. "$CONF/certs/" )
      chmod -R 600 "$CONF/certs"
      echo "certs generated. Point cert_file/key_file at ACME live paths for renewal,"
      echo "or keep these. COPY $CONF/certs/ca.crt to entry/NAT client hosts."
    else
      echo "certs already present. For ACME, set cert_file/key_file to your live paths."
    fi
    install -m 0644 "$ROOT/systemd/deepseek-server.service" "$SYSD/deepseek-server.service"
    systemctl daemon-reload
    systemctl enable --now deepseek-server
    systemctl status deepseek-server --no-pager || true
    echo
    echo ">> Web console: https://<this-server>:8443  (goes through first-run init; ephemeral self-signed TLS by default)"
    ;;
  client)
    RUN_USER="${ARG2:-$(id -un)}"
    install -m 0644 "$ROOT/configs/client.json" "$CONF/client.json"
    install -m 0644 "$ROOT/systemd/deepseek-client@.service" "$SYSD/deepseek-client@.service"
    echo "edit $CONF/client.json (server, TLS trust), then:"
    echo "  sudo systemctl daemon-reload"
    echo "  sudo systemctl enable --now deepseek-client@$RUN_USER"
    ;;
  natclient)
    CID="${ARG2:?natclient requires a CLIENT_ID as 2nd arg}"
    RUN_USER="${ARG3:-$(id -un)}"
    install -m 0644 "$ROOT/configs/natclient.json" "$CONF/natclient-$CID.json"
    install -m 0644 "$ROOT/systemd/deepseek-natclient@.service" "$SYSD/deepseek-natclient@.service"
    echo "edit $CONF/natclient-$CID.json (client_id, secret, services, server, TLS trust), then:"
    echo "  sudo systemctl daemon-reload"
    echo "  sudo systemctl enable --now deepseek-natclient@$CID"
    ;;
esac
