This directory should hold the certificates used by the DMS (docker mail server) for secure IMAP and SMTP. If you do not have one, you can generate one using the following the openssl command described in the [docker mail server documentation](../README.md#using-self-signed-certificates) or in the email server tab of the [Configurator UI](../../../scripts/configurator/README.md).

### Full chain certificate

- SSL_CERT_PATH=/tmp/dms/custom-certs/fullchain.pem

### Private key

- SSL_KEY_PATH=/tmp/dms/custom-certs/privkey.pem