# Agent IMAP/SMTP Server

A mail server for agent communication. This server is provided as an option for agents to use. Agents can use any email server provided that they can connect to it via IMAP and SMTP.

As an extra security precaution, it is recommended to use a dedicated email domain for agent communication. The example configuration provided here ensures that no email is received or delivered outside the company and agent email domains.

If you do use this server, be sure to properly configure the email domain for delivery (SPF, DKIM, DMARC suggested).

See the config directory for additional domain configuration.

## Docker

```bash
cp docker-compose.yml.example docker-compose.yml
```

Edit the `docker-compose.yml` file to configure the email domain:

```dockerfile
#   Change the hostname and domainname to match your case
    hostname: mail-agents.example.com
    domainname: agents.example.com
```

```
docker compose up -d
```

## Managing Agent Email Accounts

Connect to the container and run the following commands:

```bash
# Connect to the container
docker exec -it dms bash

# Create accounts (create your own email addresses and passwords)
root@mele:/# setup email add kiku@agents.mxhero.com "pass"
root@mele:/# setup email add beta@agents.mxhero.com "pass"
root@mele:/# setup email add gamma@agents.mxhero.com "pass"
root@mele:/# setup email add delta@agents.mxhero.com "pass"

# Delete accounts
root@mele:/# setup email del alpha@example.com

# Updating accounts
root@mele:/# setup email update alpha@agents.mxhero.com "newpass"

# List accounts
root@mele:/# setup email list

# Recreating
docker rm -f dms && docker compose up --force-recreate
```

```bash
# TESTING
openssl s_client -connect agents.mxhero.com:993 -quiet
```

## Log analysis

```bash
docker exec -it dms cat /var/log/mail/mail.log > /tmp/log.txt
# look for errors
grep postfix/error /tmp/log.txt 
```