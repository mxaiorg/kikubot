# DMS (Docker Mailserver) Config

> Configuration of DMS (Docker Mailserver) can also be done via the Configuration UI or via your Coding LLM. See the project's [README](../../../README.md#prerequisites) for more details.

In the example configuration files, agents are in the `agents.example.com` domain and are only able to receive from and send to the domain `example.com` and `agents.example.com`.

If you're not using the Configurator, copy the example configuration files in the config directory, removing '-example'.
```bash
cp postfix-sender-access-example.cf postfix-sender-access.cf
cp postfix-transport-example.cf postfix-transport.cf
```

Replace the `example.com` with your own domain name. Replace the `agents.example.com` with your own agents' subdomain.

You could, of course, use your organization's domain, but having a specific domain for the agents allows for easier isolation and inspection of agent activity.

## DKIM signing config (`rspamd/override.d/`)

`rspamd/override.d/dkim_signing-example.conf` is a template for rspamd's DKIM signing with the one setting DMS gets wrong for this layout already fixed: `use_esld = false`, required for agents on a subdomain. You normally don't copy it — `../dkim-setup.sh <domain>` generates the live `dkim_signing.conf` and patches it. If you prefer to copy it, do so **before** running `setup config dkim` and replace `agents.example.com`; DMS keeps the existing file and only adds the key. The live `dkim_signing.conf` and the `dkim/*.private.txt` keys are gitignored. See [README-SPF_DKIM_etc.md](../README-SPF_DKIM_etc.md).
