package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coredns/coredns/pb"
	"github.com/miekg/dns"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultListenAddr   = ":8080"
	defaultCoreDNSAddr  = "127.0.0.1:1053"
	defaultQueryTimeout = 2 * time.Second
)

type routerServer struct {
	dnsClient  pb.DnsServiceClient
	timeout    time.Duration
	forceHTTPS bool
}

func main() {
	listenAddr := flag.String("listen-addr", defaultListenAddr, "HTTP listen address")
	coreDNSAddr := flag.String("coredns-grpc-addr", defaultCoreDNSAddr, "CoreDNS gRPC endpoint")
	queryTimeout := flag.Duration("query-timeout", defaultQueryTimeout, "DNS query timeout")
	forceHTTPS := flag.Bool("force-https", false, "Always redirect to https scheme")
	grpcUseTLS := flag.Bool("grpc-use-tls", false, "Use TLS for CoreDNS gRPC connection")
	grpcCertFile := flag.String("grpc-cert-file", "", "Client certificate file for CoreDNS gRPC mTLS")
	grpcKeyFile := flag.String("grpc-key-file", "", "Client key file for CoreDNS gRPC mTLS")
	grpcCAFile := flag.String("grpc-ca-file", "", "CA certificate file for CoreDNS gRPC TLS verification")
	flag.Parse()

	transportCreds := credentials.TransportCredentials(insecure.NewCredentials())
	if *grpcUseTLS {
		tlsCreds, err := loadGRPCTLSCredentials(*coreDNSAddr, *grpcCertFile, *grpcKeyFile, *grpcCAFile)
		if err != nil {
			log.Fatalf("failed to load gRPC TLS credentials: %v", err)
		}
		transportCreds = tlsCreds
	}

	conn, err := grpc.NewClient(*coreDNSAddr, grpc.WithTransportCredentials(transportCreds))
	if err != nil {
		log.Fatalf("failed to connect to CoreDNS gRPC endpoint %q: %v", *coreDNSAddr, err)
	}
	defer conn.Close()

	srv := &routerServer{
		dnsClient:  pb.NewDnsServiceClient(conn),
		timeout:    *queryTimeout,
		forceHTTPS: *forceHTTPS,
	}

	httpServer := &http.Server{
		Addr:              *listenAddr,
		Handler:           http.HandlerFunc(srv.handleRedirect),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf(
		"http-router listening on %s (CoreDNS gRPC: %s, force-https: %t, grpc-tls: %t)",
		*listenAddr,
		*coreDNSAddr,
		*forceHTTPS,
		*grpcUseTLS,
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server failed: %v", err)
	}
}

func loadGRPCTLSCredentials(targetAddr, certFile, keyFile, caFile string) (credentials.TransportCredentials, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	host := hostOnly(targetAddr)
	if host != "" {
		tlsConfig.ServerName = host
	}

	if caFile != "" {
		pemData, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file %q: %w", caFile, err)
		}

		pool := x509.NewCertPool()
		if ok := pool.AppendCertsFromPEM(pemData); !ok {
			return nil, fmt.Errorf("parse CA file %q: no certificates found", caFile)
		}
		tlsConfig.RootCAs = pool
	}

	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, fmt.Errorf("both grpc-cert-file and grpc-key-file are required for mTLS")
		}

		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate/key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return credentials.NewTLS(tlsConfig), nil
}

func (s *routerServer) handleRedirect(w http.ResponseWriter, r *http.Request) {
	incomingHost := hostOnly(r.Host)
	if incomingHost == "" {
		http.Error(w, "missing host header", http.StatusBadRequest)
		return
	}

	clientIP := requestClientIP(r)

	redirectHost, err := s.resolveRedirectHost(r.Context(), incomingHost, clientIP)
	if err != nil {
		log.Printf("dns lookup failed for %q: %v", incomingHost, err)
		http.Error(w, "failed to resolve cache node", http.StatusBadGateway)
		return
	}

	scheme := requestScheme(r, s.forceHTTPS)
	targetURL := fmt.Sprintf("%s://%s%s", scheme, redirectHost, r.URL.RequestURI())
	http.Redirect(w, r, targetURL, http.StatusFound)
}

func (s *routerServer) resolveRedirectHost(parentCtx context.Context, host string, clientIP net.IP) (string, error) {
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(host), dns.TypeA)
	msg.RecursionDesired = true
	addClientSubnet(msg, clientIP)

	wire, err := msg.Pack()
	if err != nil {
		return "", fmt.Errorf("pack dns query: %w", err)
	}

	ctx, cancel := context.WithTimeout(parentCtx, s.timeout)
	defer cancel()

	resp, err := s.dnsClient.Query(ctx, &pb.DnsPacket{Msg: wire})
	if err != nil {
		return "", fmt.Errorf("grpc query failed: %w", err)
	}

	answer := new(dns.Msg)
	if err := answer.Unpack(resp.GetMsg()); err != nil {
		return "", fmt.Errorf("unpack dns response: %w", err)
	}

	var cnameTarget string
	for _, rr := range answer.Answer {
		switch record := rr.(type) {
		case *dns.CNAME:
			target := strings.TrimSuffix(record.Target, ".")
			if target != "" {
				cnameTarget = target
			}
		case *dns.A:
			if cnameTarget != "" {
				continue
			}

			target := strings.TrimSuffix(record.Hdr.Name, ".")
			if target == "" {
				continue
			}
			return target, nil
		}
	}

	if cnameTarget != "" {
		return cnameTarget, nil
	}

	return "", fmt.Errorf("no A or CNAME record in DNS answer for %q", host)
}

func addClientSubnet(msg *dns.Msg, clientIP net.IP) {
	if clientIP == nil {
		return
	}

	opt := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}}
	subnet := &dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET}

	if ip4 := clientIP.To4(); ip4 != nil {
		subnet.Family = 1
		subnet.SourceNetmask = 32
		subnet.Address = ip4
	} else {
		subnet.Family = 2
		subnet.SourceNetmask = 128
		subnet.Address = clientIP.To16()
	}

	opt.Option = append(opt.Option, subnet)
	msg.Extra = append(msg.Extra, opt)
}

func requestClientIP(r *http.Request) net.IP {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		for part := range strings.SplitSeq(xff, ",") {
			candidate := strings.TrimSpace(part)
			if candidate == "" {
				continue
			}
			if ip := net.ParseIP(candidate); ip != nil {
				return ip
			}
		}
	}

	host := r.RemoteAddr
	if parsedHost, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = parsedHost
	}

	return net.ParseIP(strings.TrimSpace(host))
}

func requestScheme(r *http.Request, forceHTTPS bool) string {
	if forceHTTPS {
		return "https"
	}
	if xfProto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xfProto != "" {
		return xfProto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func hostOnly(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return ""
	}

	host, _, err := net.SplitHostPort(hostport)
	if err == nil {
		return host
	}

	if strings.Contains(err.Error(), "missing port in address") {
		return hostport
	}

	return hostport
}
