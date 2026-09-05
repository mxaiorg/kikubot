#!/usr/bin/env bash
#
# dkim-setup.sh — generate a DKIM key in the DMS container, make rspamd
# actually sign with it, verify, and print the DNS TXT record to publish.
#
# Why this exists. `setup config dkim` (DMS's built-in helper) writes a
# dkim_signing.conf with `use_esld = true`. That tells rspamd to reduce the
# From: domain to its effective second-level domain before looking it up in
# the `domain {}` map — so mail from alpha@agents.acme.com is looked up as
# `acme.com`, which isn't in the map, and rspamd silently skips signing (the
# skip is only logged at debug level). This repo recommends a dedicated agent
# subdomain, so every deployment that follows our own advice hits it. The fix
# is `use_esld = false`; this script applies it right after key generation so
# nobody has to discover it the hard way.
#
# Steps:
#   1. docker exec <container> setup config dkim domain <domain> …
#   2. set use_esld = false in config/rspamd/override.d/dkim_signing.conf
#   3. restart the container (override.d changes need a restart, unlike keys)
#   4. confirm the running config: use_esld false, domain in the map
#   5. confirm the private key is readable by rspamd's user (_rspamd)
#   6. print the TXT record via dkim-txt.sh (folded into 255-byte strings)
#
# Re-running is safe: an existing key is kept (DMS refuses to overwrite it)
# and an already-patched config is left alone.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_DIR="${CONFIG_DIR:-$SCRIPT_DIR/config}"
SIGNING_CONF="$CONFIG_DIR/rspamd/override.d/dkim_signing.conf"
DKIM_DIR="$CONFIG_DIR/rspamd/dkim"
CONTAINER="${DMS_CONTAINER:-dms}"

domain=""
selector=mail
keysize=2048
keytype=rsa
no_restart=0
fix_only=0

usage() {
  cat <<'USAGE'
Usage: dkim-setup.sh [options] <domain>

Generates a DKIM key inside the DMS container, patches rspamd so it signs mail
for a subdomain (use_esld = false), restarts the container, verifies the live
config, and prints the DNS TXT record to publish.

  dkim-setup.sh agents.acme.com
  dkim-setup.sh -s 2026a agents.acme.com        different selector
  dkim-setup.sh --fix-only agents.acme.com      key already exists: just patch + verify

Options:
  -s, --selector <sel>   DKIM selector (default: mail)
  -k, --keysize <bits>   RSA key size (default: 2048)
  -t, --keytype <type>   rsa | ed25519 (default: rsa)
      --fix-only         skip key generation; patch use_esld, verify, print record
      --no-restart       patch the file but do not restart the container
                         (you must restart before signing takes effect)
  -h, --help             this text

Environment:
  DMS_CONTAINER   container name (default: dms — the container_name in
                  docker-compose.yml; the compose *service* is `mailserver`)
  CONFIG_DIR      host path bind-mounted at /tmp/docker-mailserver
                  (default: ./config next to this script)
USAGE
}

die()  { printf 'dkim-setup: %s\n' "$*" >&2; exit 1; }
step() { printf '\n==> %s\n' "$*" >&2; }
ok()   { printf '    ok: %s\n' "$*" >&2; }
warn() { printf '    warning: %s\n' "$*" >&2; }

while [ $# -gt 0 ]; do
  case "$1" in
    -s|--selector) [ $# -ge 2 ] || die "--selector needs an argument"; selector="$2"; shift 2 ;;
    -k|--keysize)  [ $# -ge 2 ] || die "--keysize needs an argument";  keysize="$2";  shift 2 ;;
    -t|--keytype)  [ $# -ge 2 ] || die "--keytype needs an argument";  keytype="$2";  shift 2 ;;
    --fix-only)    fix_only=1; shift ;;
    --no-restart)  no_restart=1; shift ;;
    -h|--help)     usage; exit 0 ;;
    -*)            printf 'dkim-setup: unknown option: %s\n\n' "$1" >&2; usage >&2; exit 2 ;;
    *)             [ -z "$domain" ] || die "more than one domain given ($domain, $1)"; domain="$1"; shift ;;
  esac
done

[ -n "$domain" ] || { usage >&2; exit 2; }
case "$domain" in
  *.*) ;;
  *)   die "'$domain' does not look like a domain" ;;
esac

command -v docker >/dev/null 2>&1 || die "docker not found on PATH"
[ -x "$SCRIPT_DIR/dkim-txt.sh" ] || die "dkim-txt.sh not found next to this script"

if ! docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null | grep -q true; then
  die "container '$CONTAINER' is not running.
  Start it first:   cd $SCRIPT_DIR && docker compose up -d
  (the compose service is 'mailserver'; the container name is '$CONTAINER'.
   Set DMS_CONTAINER if you renamed it.)"
fi

# --- 1. generate -------------------------------------------------------------
if [ "$fix_only" -eq 0 ]; then
  step "Generating DKIM key for $domain (selector $selector, $keytype $keysize) in container $CONTAINER"
  # No -t: this is not interactive, and a TTY would garble the output when piped.
  docker exec "$CONTAINER" setup config dkim keytype "$keytype" keysize "$keysize" selector "$selector" domain "$domain" >&2 \
    || die "setup config dkim failed (see output above)"
else
  step "Skipping key generation (--fix-only)"
fi

[ -f "$SIGNING_CONF" ] || die "expected $SIGNING_CONF to exist after key generation but it does not.
  Is $CONFIG_DIR the directory bind-mounted at /tmp/docker-mailserver in docker-compose.yml?
  If the container was started with ENABLE_OPENDKIM=1 you are on the OpenDKIM signer, which this script does not manage."

shopt -s nullglob
priv=( "$DKIM_DIR"/*-"$selector"-"$domain".private.txt )
shopt -u nullglob
[ "${#priv[@]}" -gt 0 ] || die "no private key for $domain / selector $selector under $DKIM_DIR"
ok "private key: ${priv[0]##*/}"

grep -qE "^[[:space:]]*${domain//./\\.}[[:space:]]*\{" "$SIGNING_CONF" \
  || die "$domain has no entry in $SIGNING_CONF — did key generation succeed?"

# --- 2. patch use_esld -------------------------------------------------------
step "Making rspamd sign for a subdomain (use_esld = false) in ${SIGNING_CONF#$SCRIPT_DIR/}"
if grep -qE '^[[:space:]]*use_esld[[:space:]]*=[[:space:]]*false[[:space:]]*;' "$SIGNING_CONF"; then
  ok "already set"
  patched=0
elif grep -qE '^[[:space:]]*use_esld[[:space:]]*=[[:space:]]*true[[:space:]]*;' "$SIGNING_CONF"; then
  # Portable in-place edit (BSD and GNU sed differ on -i).
  tmp="$(mktemp)"
  sed -E 's/^([[:space:]]*use_esld[[:space:]]*=[[:space:]]*)true([[:space:]]*;.*)$/\1false\2/' "$SIGNING_CONF" > "$tmp"
  cat "$tmp" > "$SIGNING_CONF"   # cat, not mv: keep the file's owner/mode
  rm -f "$tmp"
  ok "changed true -> false"
  patched=1
else
  # Not present at all: rspamd's default is true, so add it explicitly.
  printf '\n# Added by dkim-setup.sh: sign for subdomains (do not reduce From: to the eSLD).\nuse_esld = false;\n' >> "$SIGNING_CONF"
  ok "added (was not set; rspamd defaults to true)"
  patched=1
fi

# --- 3. restart --------------------------------------------------------------
if [ "$patched" -eq 1 ] || [ "$fix_only" -eq 1 ]; then
  if [ "$no_restart" -eq 1 ]; then
    warn "--no-restart given: the running container still has the old config.
    Restart before testing:  docker restart $CONTAINER"
  else
    step "Restarting $CONTAINER so rspamd loads the new override"
    docker restart "$CONTAINER" >/dev/null || die "docker restart $CONTAINER failed"
    printf '    waiting for rspamd' >&2
    for _ in $(seq 1 60); do
      if docker exec "$CONTAINER" rspamadm configdump dkim_signing >/dev/null 2>&1; then break; fi
      printf '.' >&2; sleep 2
    done
    printf '\n' >&2
  fi
fi

# --- 4. verify running config -----------------------------------------------
step "Verifying the running rspamd config"
dump="$(docker exec "$CONTAINER" rspamadm configdump dkim_signing 2>/dev/null || true)"
[ -n "$dump" ] || die "rspamadm configdump dkim_signing produced nothing — is rspamd up? (ENABLE_RSPAMD=1 in docker-compose.yml?)"

if printf '%s\n' "$dump" | grep -qE 'use_esld[[:space:]]*=[[:space:]]*false'; then
  ok "use_esld = false"
elif [ "$no_restart" -eq 1 ]; then
  warn "running config still shows use_esld = true (expected until you restart)"
else
  die "running config still shows use_esld = true after restart.
  Check that $CONFIG_DIR is the directory mounted at /tmp/docker-mailserver, then:
    docker exec $CONTAINER rspamadm configdump dkim_signing"
fi

if printf '%s\n' "$dump" | grep -qF "$domain"; then
  ok "$domain is in the signing domain map"
else
  die "$domain is not in rspamd's running dkim_signing domain map"
fi

if printf '%s\n' "$dump" | grep -qE 'enabled[[:space:]]*=[[:space:]]*false'; then
  die "dkim_signing module is disabled (enabled = false) in the running config"
fi

# --- 5. key readable by _rspamd ---------------------------------------------
step "Checking the private key is readable by rspamd inside the container"
key_in_ctr="/tmp/docker-mailserver/rspamd/dkim/${priv[0]##*/}"
if docker exec "$CONTAINER" su -s /bin/sh _rspamd -c "test -r '$key_in_ctr'" 2>/dev/null; then
  ok "_rspamd can read ${priv[0]##*/}"
else
  warn "_rspamd cannot read $key_in_ctr — signing will be skipped silently. Fix with:
    docker exec $CONTAINER chown _rspamd:_rspamd /tmp/docker-mailserver/rspamd/dkim/*
    docker exec $CONTAINER chmod 0640 /tmp/docker-mailserver/rspamd/dkim/*.private.txt"
fi

# --- 6. DNS record -----------------------------------------------------------
step "DNS TXT record to publish (value on stdout, notes on stderr)"
DKIM_DIR="$DKIM_DIR" "$SCRIPT_DIR/dkim-txt.sh" -s "$selector" "$domain"

cat >&2 <<EOF

Next:
  1. Publish the record above at your DNS provider (one record, quotes included).
  2. Verify DNS:      dig +short TXT ${selector}._domainkey.${domain}
  3. Send a test from an agent account to check-auth@verifier.port25.com, then:
       docker exec $CONTAINER sh -c 'grep -E "DKIM_SIGNED|LOCAL_OUTBOUND" /var/log/mail/rspamd.log | tail -5'
     You want DKIM_SIGNED(...){${domain}:s=${selector};} next to LOCAL_OUTBOUND.
     LOCAL_OUTBOUND alone, with no error, means rspamd ran and declined to sign —
     see "Prove that rspamd is signing" in README-SPF_DKIM_etc.md.
EOF
