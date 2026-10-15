package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	dnsClient pb.DnsServiceClient
	timeout   time.Duration
}

func main() {
	listenAddr := flag.String("listen-addr", defaultListenAddr, "HTTP listen address")
	coreDNSAddr := flag.String("coredns-grpc-addr", defaultCoreDNSAddr, "CoreDNS gRPC endpoint")
	queryTimeout := flag.Duration("query-timeout", defaultQueryTimeout, "DNS query timeout")
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
		dnsClient: pb.NewDnsServiceClient(conn),
		timeout:   *queryTimeout,
	}

	httpServer := &http.Server{
		Addr:              *listenAddr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf(
		"http-router API listening on %s (CoreDNS gRPC: %s, grpc-tls: %t)",
		*listenAddr,
		*coreDNSAddr,
		*grpcUseTLS,
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server failed: %v", err)
	}
}

func (s *routerServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/route", methodOnly(http.MethodPost, s.handleRoute))
	mux.HandleFunc("/healthz", methodOnly(http.MethodGet, handleHealth))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "not found"})
	})
	return mux
}

func methodOnly(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
			return
		}
		next(w, r)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func loadGRPCTLSCredentials(targetAddr, certFile, keyFile, caFile string) (credentials.TransportCredentials, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if host, _, err := net.SplitHostPort(targetAddr); err == nil && host != "" {
		tlsConfig.ServerName = host
	} else if targetAddr != "" {
		tlsConfig.ServerName = targetAddr
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

type routeRequest struct {
	Domain     string `json:"domain"`
	SourceIP   string `json:"source_ip"`
	RecordType string `json:"record_type"`
}

func queryType(recordType string) (uint16, bool) {
	switch strings.ToUpper(strings.TrimSpace(recordType)) {
	case "", "A":
		return dns.TypeA, true
	case "AAAA":
		return dns.TypeAAAA, true
	case "CNAME":
		return dns.TypeCNAME, true
	case "TXT":
		return dns.TypeTXT, true
	case "MX":
		return dns.TypeMX, true
	case "SRV":
		return dns.TypeSRV, true
	case "NS":
		return dns.TypeNS, true
	default:
		return 0, false
	}
}

type routeResponse struct {
	Domain   string       `json:"domain"`
	SourceIP string       `json:"source_ip,omitempty"`
	Response *dnsResponse `json:"response"`
}

type dnsResponse struct {
	ID                 uint16        `json:"id"`
	Opcode             string        `json:"opcode"`
	Rcode              string        `json:"rcode"`
	Authoritative      bool          `json:"authoritative"`
	Truncated          bool          `json:"truncated"`
	RecursionDesired   bool          `json:"recursion_desired"`
	RecursionAvailable bool          `json:"recursion_available"`
	AuthenticatedData  bool          `json:"authenticated_data"`
	CheckingDisabled   bool          `json:"checking_disabled"`
	Question           []dnsQuestion `json:"question"`
	Answer             []dnsRR       `json:"answer"`
	Authority          []dnsRR       `json:"authority"`
	Additional         []dnsRR       `json:"additional"`
	EDNS               *dnsEDNS      `json:"edns,omitempty"`
}

type dnsQuestion struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Class string `json:"class"`
}

type dnsRR struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Class string `json:"class"`
	TTL   uint32 `json:"ttl"`
	Data  string `json:"data"`
}

type dnsEDNS struct {
	Version       uint8           `json:"version"`
	UDPSize       uint16          `json:"udp_size"`
	DNSSECOk      bool            `json:"dnssec_ok"`
	ExtendedRcode int             `json:"extended_rcode"`
	Options       []dnsEDNSOption `json:"options"`
}

type dnsEDNSOption struct {
	Code  uint16 `json:"code"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type errorResponse struct {
	Error string `json:"error"`
}

const maxRouteBodyBytes = 4096

func (s *routerServer) handleRoute(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRouteBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req routeRequest
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("invalid JSON body: %v", err)})
		return
	}

	domain := strings.TrimSpace(req.Domain)
	if domain == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "domain is required"})
		return
	}
	if _, ok := dns.IsDomainName(domain); !ok {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("invalid domain %q", domain)})
		return
	}

	var sourceIP net.IP
	if raw := strings.TrimSpace(req.SourceIP); raw != "" {
		sourceIP = net.ParseIP(raw)
		if sourceIP == nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("invalid source_ip %q", raw)})
			return
		}
	}

	qtype, ok := queryType(req.RecordType)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "record_type must be A, AAAA, CNAME, TXT, MX, SRV, or NS"})
		return
	}
	answer, err := s.queryDNS(r.Context(), domain, sourceIP, qtype)
	if err != nil {
		log.Printf("dns lookup failed for %q: %v", domain, err)
		writeJSON(w, http.StatusBadGateway, errorResponse{Error: "failed to query DNS"})
		return
	}

	resp := routeResponse{
		Domain:   dns.Fqdn(domain),
		Response: dnsMsgToJSON(answer),
	}
	if sourceIP != nil {
		resp.SourceIP = sourceIP.String()
	}

	writeJSON(w, http.StatusOK, resp)
}

func dnsMsgToJSON(m *dns.Msg) *dnsResponse {
	out := &dnsResponse{
		ID:                 m.Id,
		Opcode:             dns.OpcodeToString[m.Opcode],
		Rcode:              dns.RcodeToString[m.Rcode],
		Authoritative:      m.Authoritative,
		Truncated:          m.Truncated,
		RecursionDesired:   m.RecursionDesired,
		RecursionAvailable: m.RecursionAvailable,
		AuthenticatedData:  m.AuthenticatedData,
		CheckingDisabled:   m.CheckingDisabled,
		Question:           make([]dnsQuestion, 0, len(m.Question)),
		Answer:             rrsToJSON(m.Answer),
		Authority:          rrsToJSON(m.Ns),
		Additional:         make([]dnsRR, 0, len(m.Extra)),
	}

	for _, q := range m.Question {
		out.Question = append(out.Question, dnsQuestion{
			Name:  q.Name,
			Type:  dns.TypeToString[q.Qtype],
			Class: dns.ClassToString[q.Qclass],
		})
	}

	for _, rr := range m.Extra {
		if opt, ok := rr.(*dns.OPT); ok {
			out.EDNS = ednsToJSON(opt)
			continue
		}
		out.Additional = append(out.Additional, rrToJSON(rr))
	}

	return out
}

func rrsToJSON(rrs []dns.RR) []dnsRR {
	out := make([]dnsRR, 0, len(rrs))
	for _, rr := range rrs {
		out = append(out, rrToJSON(rr))
	}
	return out
}

func rrToJSON(rr dns.RR) dnsRR {
	hdr := rr.Header()
	return dnsRR{
		Name:  hdr.Name,
		Type:  dns.TypeToString[hdr.Rrtype],
		Class: dns.ClassToString[hdr.Class],
		TTL:   hdr.Ttl,
		Data:  strings.TrimPrefix(rr.String(), hdr.String()),
	}
}

func ednsToJSON(opt *dns.OPT) *dnsEDNS {
	out := &dnsEDNS{
		Version:       opt.Version(),
		UDPSize:       opt.UDPSize(),
		DNSSECOk:      opt.Do(),
		ExtendedRcode: opt.ExtendedRcode(),
		Options:       make([]dnsEDNSOption, 0, len(opt.Option)),
	}
	for _, o := range opt.Option {
		name := fmt.Sprintf("OPT%d", o.Option())
		switch o.(type) {
		case *dns.EDNS0_SUBNET:
			name = "ECS"
		case *dns.EDNS0_COOKIE:
			name = "COOKIE"
		case *dns.EDNS0_NSID:
			name = "NSID"
		case *dns.EDNS0_PADDING:
			name = "PADDING"
		case *dns.EDNS0_EDE:
			name = "EDE"
		}
		out.Options = append(out.Options, dnsEDNSOption{
			Code:  o.Option(),
			Name:  name,
			Value: o.String(),
		})
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("failed to write JSON response: %v", err)
	}
}

func (s *routerServer) queryDNS(parentCtx context.Context, host string, clientIP net.IP, qtype uint16) (*dns.Msg, error) {
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(host), qtype)
	msg.RecursionDesired = true
	addClientSubnet(msg, clientIP)

	wire, err := msg.Pack()
	if err != nil {
		return nil, fmt.Errorf("pack dns query: %w", err)
	}

	ctx, cancel := context.WithTimeout(parentCtx, s.timeout)
	defer cancel()

	resp, err := s.dnsClient.Query(ctx, &pb.DnsPacket{Msg: wire})
	if err != nil {
		return nil, fmt.Errorf("grpc query failed: %w", err)
	}

	answer := new(dns.Msg)
	if err := answer.Unpack(resp.GetMsg()); err != nil {
		return nil, fmt.Errorf("unpack dns response: %w", err)
	}

	return answer, nil
}

func addClientSubnet(msg *dns.Msg, clientIP net.IP) {
	if clientIP == nil {
		return
	}

	opt := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}}
	opt.SetUDPSize(dns.DefaultMsgSize)
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
