# HTTP Router

Small HTTP redirector that asks CoreDNS (via gRPC) where a request should be sent.

For each incoming request host:

1. Build a DNS `A` query using `github.com/miekg/dns`.
2. Add EDNS0 client subnet (ECS) from request source IP.
3. If `X-Forwarded-For` is present, use the first valid IP from that header for ECS.
4. Send the packed DNS message to CoreDNS gRPC `DnsService.Query` (`github.com/coredns/coredns/pb`).
5. Read the first `A` answer and use its owner name as the cache node host.
6. Return `302 Found` with `Location: <scheme>://<cache-node-host><original-path-and-query>`.

## Configuration

Use command-line flags:

- `-listen-addr` (default `:8080`) HTTP bind address.
- `-coredns-grpc-addr` (default `127.0.0.1:1053`) CoreDNS gRPC endpoint.
- `-query-timeout` (default `2s`) DNS query timeout.
- `-force-https` (default `false`) always use `https` in redirects.
- `-grpc-use-tls` (default `false`) use TLS when connecting to CoreDNS gRPC.
- `-grpc-cert-file` client certificate for gRPC mTLS.
- `-grpc-key-file` client key for gRPC mTLS.
- `-grpc-ca-file` CA bundle used to verify the CoreDNS gRPC server cert.

## Run

```bash
go run . -listen-addr :8080 -coredns-grpc-addr 127.0.0.1:1053
```

Then send a request with the desired host:

```bash
curl -i -H 'Host: app.example.com' http://127.0.0.1:8080/some/path
```

If CoreDNS returns an `A` record with owner name `node1.frankfurt.node.app.example.com.`,
the response includes:

- Status: `302 Found`
- Location: `http://node1.frankfurt.node.app.example.com/some/path`
