FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o kiku ./cmd/kikubot

FROM alpine:latest

RUN apk add --no-cache ca-certificates nodejs npm bash curl jq

# Pre-install MCP server & CLI packages so npx doesn't download them at runtime
#RUN npm install --global @tsmztech/mcp-server-salesforce
#RUN npm install -g @xeroapi/xero-mcp-server

# https://developer.box.com/guides/cli/cli-with-jwt-authentication/jwt-cli
# https://developer.box.com/guides/cli/cli-with-jwt-authentication/jwt-cli
# Box CLI auth: copy your Box app services JSON into the image, then
# uncomment the following line to register it as an environment.
#RUN npm install --global @box/cli
#COPY box_config.json /app/box_config.json
#RUN npx -y @box/cli configure:environments:add /app/box_config.json

WORKDIR /app
COPY --from=builder /app/kiku .
# Copy knowledge files if they exist; the app handles missing dirs gracefully.
COPY --from=builder /app/configs/knowledge/ ./knowledge/
COPY --from=builder /app/configs/agents.yaml .
# Remote MCP catalog. The glob also matches the always-committed
# mcp_servers-example.yaml, so the COPY never fails when a deployment has no
# live mcp_servers.yaml; the resolver looks specifically for mcp_servers.yaml
# and degrades gracefully (no remote MCPs) when only the example is present.
COPY --from=builder /app/configs/mcp_servers*.yaml ./

CMD ["./kiku"]
