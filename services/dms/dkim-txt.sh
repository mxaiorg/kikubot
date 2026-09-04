#!/usr/bin/env bash
#
# dkim-txt.sh — turn a generated DKIM public key into a paste-ready DNS TXT value.
#
# A DNS TXT record is a sequence of character-strings, each capped at 255 bytes.
# A 2048-bit RSA DKIM key blows past that, so providers that don't split for you
# (Route 53 among them) reject the value outright. The fix is to publish ONE
# record whose value is several quoted strings separated by spaces; resolvers
# concatenate them back into one value at lookup time, which is exactly what
# DKIM verification expects. This is standard, not a workaround.
#
# The parser follows the same rule: it concatenates the contents of quoted
# strings and drops everything between them, so it handles the multi-line
# `mail._domainkey IN TXT ( "..." "..." )` file rspamd writes, the single-line
# blob `setup config dkim` prints to the console, and OpenDKIM's mail.txt.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DKIM_DIR="${DKIM_DIR:-$SCRIPT_DIR/config/rspamd/dkim}"

selector=mail
chunk=255
raw=0
file=""
domain=""

usage() {
  cat <<'USAGE'
Usage: dkim-txt.sh [options] [domain]

Reads a DKIM public key and prints the TXT record value split into quoted
255-byte strings, ready to paste into Route 53 (or any provider that makes
you split it yourself).

  dkim-txt.sh                             auto-discover the key under config/rspamd/dkim/
  dkim-txt.sh agents.acme.com             pick by domain
  dkim-txt.sh -f config/opendkim/keys/agents.acme.com/mail.txt
  docker exec dms setup config dkim domain agents.acme.com | dkim-txt.sh -

Options:
  -f, --file <path>     read the key from <path> ("-" for stdin)
  -s, --selector <sel>  selector to look for / report (default: mail)
  -c, --chunk <n>       max bytes per quoted string (default: 255)
  -r, --raw             print the single unsplit value, unquoted, for providers
                        (Cloudflare, most registrars) that chunk it themselves
  -h, --help            this text

The record value goes to stdout; the record name and verify hint go to stderr,
so `dkim-txt.sh acme.com | pbcopy` copies exactly the value and nothing else.

Override the search directory with DKIM_DIR=... if your config/ lives elsewhere.
USAGE
}

die() { printf 'dkim-txt: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    -f|--file)     [ $# -ge 2 ] || die "--file needs an argument"; file="$2"; shift 2 ;;
    -s|--selector) [ $# -ge 2 ] || die "--selector needs an argument"; selector="$2"; shift 2 ;;
    -c|--chunk)    [ $# -ge 2 ] || die "--chunk needs an argument"; chunk="$2"; shift 2 ;;
    -r|--raw)      raw=1; shift ;;
    -h|--help)     usage; exit 0 ;;
    -)             file="-"; shift ;;
    -*)            printf 'dkim-txt: unknown option: %s\n\n' "$1" >&2; usage >&2; exit 2 ;;
    *)             [ -z "$domain" ] || die "more than one domain given ($domain, $1)"; domain="$1"; shift ;;
  esac
done

case "$chunk" in
  ''|*[!0-9]*) die "--chunk must be a positive integer" ;;
  0)           die "--chunk must be a positive integer" ;;
esac

# Locate the key file when one wasn't named explicitly.
if [ -z "$file" ]; then
  [ -d "$DKIM_DIR" ] || die "no --file given and $DKIM_DIR does not exist.
  If OpenDKIM is your signer, point at it directly:
    $0 -f config/opendkim/keys/<domain>/<selector>.txt"

  shopt -s nullglob
  if [ -n "$domain" ]; then
    cands=( "$DKIM_DIR"/*-"$selector"-"$domain".public.dns.txt )
  else
    cands=( "$DKIM_DIR"/*.public.dns.txt )
  fi
  shopt -u nullglob

  if [ "${#cands[@]}" -eq 0 ]; then
    have=$(ls -1 "$DKIM_DIR" 2>/dev/null | sed 's/^/    /')
    die "no key found in $DKIM_DIR for selector '$selector'${domain:+ / domain '$domain'}.
  Directory contains:
${have:-    (empty)}"
  elif [ "${#cands[@]}" -gt 1 ]; then
    printf 'dkim-txt: %s holds keys for several domains — name one:\n' "$DKIM_DIR" >&2
    printf '    %s\n' "${cands[@]##*/}" >&2
    exit 1
  fi
  file="${cands[0]}"

  # Recover selector/domain from the filename for the informational header:
  #   rsa-2048-mail-agents.acme.com.public.dns.txt  ->  mail / agents.acme.com
  if [ -z "$domain" ]; then
    stem="${file##*/}"; stem="${stem%.public.dns.txt}"
    case "$stem" in
      rsa-*-*)    rest="${stem#rsa-*-}" ;;
      ed25519-*)  rest="${stem#ed25519-}" ;;
      *)          rest="" ;;
    esac
    # Domains may contain '-', so take only the first field as the selector.
    if [ -n "$rest" ] && [ "$rest" != "$stem" ]; then
      selector="${rest%%-*}"
      domain="${rest#*-}"
    fi
  fi
fi

if [ "$file" = "-" ]; then
  input=$(cat)
else
  [ -f "$file" ] || die "no such file: $file"
  input=$(cat -- "$file")
fi

recname=""
[ -n "$domain" ] && recname="${selector}._domainkey.${domain}"

printf '%s\n' "$input" | awk -v chunk="$chunk" -v raw="$raw" -v recname="$recname" '
  { buf = buf $0 "\n" }
  END {
    # Flatten first: some awks (BSD/one-true-awk) treat a newline as a field
    # separator in split() regardless of the separator asked for, which would
    # shred a multi-line record into misaligned fields.
    gsub(/[\n\r]/, " ", buf)

    # A TXT value is the concatenation of its quoted strings, with whatever sits
    # between them discarded. Splitting on the quote character puts the string
    # contents at the even indices.
    n = split(buf, q, "\"")
    if (n > 2) {
      for (i = 2; i <= n; i += 2) out = out q[i]
    } else {
      out = buf
      gsub(/\t/, " ", out)
      gsub(/  +/, " ", out)
    }

    # Drop any record name / "IN TXT (" preamble the file carries.
    p = index(out, "v=DKIM1")
    if (p > 1) out = substr(out, p)
    gsub(/^[ \t]+|[ \t;)]+$/, "", out)

    # Everything from p= on is base64, which never contains whitespace. Strip any
    # that survived, so unquoted line-wrapped pastes reassemble correctly too.
    pp = index(out, "p=")
    if (pp > 0) {
      head = substr(out, 1, pp + 1)
      tail = substr(out, pp + 2)
      gsub(/[ \t]/, "", tail)
      out = head tail
    }

    if (out == "" || (index(out, "v=DKIM1") == 0 && index(out, "p=") == 0)) {
      print "dkim-txt: no DKIM value found in the input (expected a v=DKIM1 record with a p= tag)" > "/dev/stderr"
      exit 1
    }
    if (index(out, "p=") == 0) {
      print "dkim-txt: warning: value has no p= tag — is this a revoked key?" > "/dev/stderr"
    }

    L = length(out)
    if (raw == 1) {
      print out
    } else {
      o = ""
      for (i = 1; i <= L; i += chunk) o = o (i == 1 ? "" : " ") "\"" substr(out, i, chunk) "\""
      print o
    }

    parts = int((L + chunk - 1) / chunk)
    if (recname != "") print "# record name:  " recname > "/dev/stderr"
    print "# record type:  TXT" > "/dev/stderr"
    if (raw == 1)
      print "# value:        1 unsplit string, " L " bytes (provider must chunk it)" > "/dev/stderr"
    else
      print "# value:        " parts " quoted string(s), " L " bytes total, chunked at " chunk > "/dev/stderr"
    print "#" > "/dev/stderr"
    if (parts > 1 && raw == 0) {
      print "# This is ONE record whose value is several quoted strings separated by" > "/dev/stderr"
      print "# single spaces — not several records. Paste the whole line, quotes included." > "/dev/stderr"
    }
    if (recname != "") print "# verify:       dig +short TXT " recname > "/dev/stderr"
  }
'
