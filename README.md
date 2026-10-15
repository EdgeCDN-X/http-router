# HTTP Router

Small JSON API that sends a DNS query to CoreDNS over gRPC and returns the parsed DNS response.

For each `POST /route` request:

1. Build a DNS `A` query for `domain` using `github.com/miekg/dns`.
2. If `source_ip` is provided, add it as an EDNS0 client subnet (ECS) option.
3. Send the packed DNS message to CoreDNS gRPC `DnsService.Query` (`github.com/coredns/coredns/pb`).
4. Return the full DNS response (header flags, question, answer, authority, additional, EDNS) as JSON.

## Endpoints

| Method | Path       | Description                         |
|--------|------------|-------------------------------------|
| POST   | `/route`   | Query DNS and return the response   |
| GET    | `/healthz` | Liveness check (`{"status":"ok"}`)  |

Any other path returns `404`. Using the wrong method on a known path returns `405` with an `Allow` header.
Every response, including errors, is JSON. Errors use the shape `{"error": "..."}`.

## Configuration

Use command-line flags:

- `-listen-addr` (default `:8080`) HTTP bind address.
- `-coredns-grpc-addr` (default `127.0.0.1:1053`) CoreDNS gRPC endpoint.
- `-query-timeout` (default `2s`) DNS query timeout.
- `-grpc-use-tls` (default `false`) use TLS when connecting to CoreDNS gRPC.
- `-grpc-cert-file` client certificate for gRPC mTLS.
- `-grpc-key-file` client key for gRPC mTLS.
- `-grpc-ca-file` CA bundle used to verify the CoreDNS gRPC server cert.

## Run

```bash
go run . -listen-addr :8080 -coredns-grpc-addr 127.0.0.1:1053
```

## `POST /route`

Request body:

- `domain` (required) the name to resolve. It is converted to an FQDN.
- `source_ip` (optional) a client IPv4 or IPv6 address. It is sent as ECS (`/32` or `/128`).

```bash
curl -s -X POST http://127.0.0.1:8080/route \
  -H 'Content-Type: application/json' \
  -d '{"domain":"app.example.com","source_ip":"1.2.3.4"}'
```

```json
{
  "domain": "app.example.com.",
  "source_ip": "1.2.3.4",
  "response": {
    "id": 4066,
    "opcode": "QUERY",
    "rcode": "NOERROR",
    "authoritative": true,
    "truncated": false,
    "recursion_desired": true,
    "recursion_available": false,
    "authenticated_data": false,
    "checking_disabled": false,
    "question": [{"name": "app.example.com.", "type": "A", "class": "IN"}],
    "answer": [
      {"name": "app.example.com.", "type": "CNAME", "class": "IN", "ttl": 30, "data": "node1.frankfurt.node.app.example.com."},
      {"name": "node1.frankfurt.node.app.example.com.", "type": "A", "class": "IN", "ttl": 30, "data": "10.0.0.1"}
    ],
    "authority": [],
    "additional": [],
    "edns": {
      "version": 0,
      "udp_size": 1232,
      "dnssec_ok": false,
      "extended_rcode": 0,
      "options": [{"code": 8, "name": "ECS", "value": "1.2.3.4/32/24"}]
    }
  }
}
```

A DNS-level failure (for example `NXDOMAIN` or `SERVFAIL`) still returns `200`, with the DNS code in `response.rcode`.
Invalid input returns `400`. A failed gRPC call returns `502`.
