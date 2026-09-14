# Agent Configurator image. Build from the kikubot project root (the module
# root), not from this directory — docker-compose-configurator.yml does this:
#
#   docker compose -f docker-compose-configurator.yml up -d --build
#
# The binary embeds its templates and static assets, but it still reads the
# project tree at runtime (configs/, services/dms/, and internal/tools source
# for the tool picker), so the compose file bind-mounts the root at /kikubot.

FROM golang:1.26-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY internal/ ./internal/
COPY scripts/configurator/ ./scripts/configurator/

RUN CGO_ENABLED=0 go build -o /configurator ./scripts/configurator

# Alpine with the docker CLI + compose plugin preinstalled: after a save the
# configurator runs `docker compose kill -s HUP` against the agents so they
# hot-reload knowledge and tools immediately (needs the host's docker.sock).
FROM docker:cli

COPY --from=builder /configurator /usr/local/bin/configurator

WORKDIR /kikubot
EXPOSE 50042

# Bind on all interfaces inside the container; the compose file publishes the
# port on the host's loopback only.
ENTRYPOINT ["configurator", "-root", "/kikubot", "-addr", "0.0.0.0", "-port", "50042"]
