# Apache Tika (REST API)

Tika handles an enormous range of formats (PDF, Word, Excel, PowerPoint, HTML, etc.) and exposes a simple HTTP API. Run it as a sidecar:

```yaml
# docker-compose.yml
services:
  tika:
    image: apache/tika:latest
    ports:
      - "9998:9998"
```

- Then from Go, just PUT the file bytes to http://tika:9998/tika and get back plain text. This is probably the most versatile option if you need broad format support.

## Documentation

[Using CURL](https://cwiki.apache.org/confluence/pages/viewpage.action?pageId=148639291#TikaServer-GettheTextofaDocument)

```bash
curl -T File.pdf http://localhost:9998/tika/text --header "Accept: application/json"
```