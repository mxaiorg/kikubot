#!/usr/bin/env bash
#
# Kikubot one-command demo launcher.
#
#   ./demo.sh           build + start the demo (greenmail + webmail + agent)
#   ./demo.sh down      stop and remove the demo containers
#
# Zero cost, fully local. No LLM key required to see the round-trip work — add
# ANTHROPIC_API_KEY to configs/demo/secrets.env for real agent replies.

set -euo pipefail
cd "$(dirname "$0")"

COMPOSE_FILE="docker-compose-demo.yml"
SECRETS="configs/demo/secrets.env"
SECRETS_EXAMPLE="configs/demo/secrets.env.example"

# Pick `docker compose` (v2) or legacy `docker-compose`.
if docker compose version >/dev/null 2>&1; then
  COMPOSE=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE=(docker-compose)
else
  echo "✗ Docker Compose not found. Install Docker Desktop and try again." >&2
  exit 1
fi

if [[ "${1:-up}" == "down" ]]; then
  exec "${COMPOSE[@]}" -f "$COMPOSE_FILE" down
fi

# Seed the secrets file on first run so the user doesn't have to.
if [[ ! -f "$SECRETS" ]]; then
  cp "$SECRETS_EXAMPLE" "$SECRETS"
  echo "→ Created $SECRETS (no LLM key — running in demo-notice mode)."
  echo "  Add ANTHROPIC_API_KEY or OPENROUTER_API_KEY there and re-run."
fi

# Pick the provider from whichever key the user pasted — no agents.yaml editing.
# Read values without sourcing the file (avoids surprises from quoting/exec).
read_key() { grep -E "^$1=" "$SECRETS" | tail -1 | cut -d= -f2- | tr -d '"'\''[:space:]'; }
ANTHROPIC_KEY="$(read_key ANTHROPIC_API_KEY)"
OPENROUTER_KEY="$(read_key OPENROUTER_API_KEY)"

# Defaults (empty) → use the anthropic provider/model baked into agents.yaml.
export LLM_PROVIDER=""
export LLM_MODEL=""
if [[ -z "$ANTHROPIC_KEY" && -n "$OPENROUTER_KEY" ]]; then
  # OpenRouter key only → switch to it. Haiku via OpenRouter is cheap and does
  # tool-calling reliably (the agent needs the report tool to reply). Override
  # the model by exporting LLM_MODEL before running, or edit agents.yaml.
  export LLM_PROVIDER="openrouter"
  export LLM_MODEL="${LLM_MODEL:-anthropic/claude-haiku-4.5}"
  echo "→ Detected OpenRouter key — using provider=openrouter, model=$LLM_MODEL"
elif [[ -n "$ANTHROPIC_KEY" ]]; then
  echo "→ Detected Anthropic key — using the agents.yaml default (anthropic/haiku)."
else
  echo "→ No LLM key set — Kiku will reply with the demo notice (zero cost)."
fi

echo "→ Building and starting the demo (first build downloads images)…"
"${COMPOSE[@]}" -f "$COMPOSE_FILE" up --build -d

cat <<'EOF'

✅ Kikubot demo is up.

  1. Open the webmail UI:   http://localhost:8000
  2. Log in:                user = human@demo.local   password = (anything)
  3. Compose a new email to: kiku@demo.local   →   send it
  4. Wait ~30s, refresh the inbox — Kiku replies. 🎉

Logs:   docker compose -f docker-compose-demo.yml logs -f kiku-demo
Stop:   ./demo.sh down
EOF
