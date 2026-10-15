package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coredns/coredns/pb"
	"github.com/miekg/dns"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type testDNS struct {
	pb.UnimplementedDnsServiceServer
}

func (testDNS) Query(_ context.Context, packet *pb.DnsPacket) (*pb.DnsPacket, error) {
	q := new(dns.Msg)
	if err := q.Unpack(packet.Msg); err != nil {
		return nil, err
	}
	answer := new(dns.Msg)
	answer.SetReply(q)
	data := map[uint16]string{
		dns.TypeA: "192.0.2.1", dns.TypeAAAA: "2001:db8::1",
		dns.TypeCNAME: "target.example.com.", dns.TypeTXT: `"verification text"`,
		dns.TypeMX: "10 mail.example.com.", dns.TypeSRV: "10 20 443 service.example.com.",
		dns.TypeNS: "ns1.example.com.",
	}
	rr, err := dns.NewRR(q.Question[0].Name + " 60 IN " + dns.TypeToString[q.Question[0].Qtype] + " " + data[q.Question[0].Qtype])
	if err != nil {
		return nil, err
	}
	answer.Answer = []dns.RR{rr}
	wire, err := answer.Pack()
	return &pb.DnsPacket{Msg: wire}, err
}

func TestRouteRecordTypes(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterDnsServiceServer(server, testDNS{})
	go server.Serve(listener)
	defer server.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	router := &routerServer{dnsClient: pb.NewDnsServiceClient(conn), timeout: time.Second}
	for _, recordType := range []string{"", "A", "AAAA", "CNAME", "TXT", "MX", "SRV", "NS", " txt "} {
		t.Run(recordType, func(t *testing.T) {
			domain := "app.example.com"
			if recordType == "SRV" {
				domain = "_sip._tcp.example.com"
			}
			body := `{"domain":"` + domain + `","record_type":"` + recordType + `"}`
			w := httptest.NewRecorder()
			router.routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/route", strings.NewReader(body)))
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			var result routeResponse
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			want := strings.ToUpper(strings.TrimSpace(recordType))
			if want == "" {
				want = "A"
			}
			if result.Response == nil || len(result.Response.Question) != 1 || len(result.Response.Answer) != 1 {
				t.Fatalf("unexpected response: %s", w.Body)
			}
			if result.Response.Question[0].Type != want || result.Response.Answer[0].Type != want ||
				result.Response.Question[0].Name != dns.Fqdn(domain) || result.Response.Answer[0].Data == "" {
				t.Fatalf("incorrect query or record: %s", w.Body)
			}
		})
	}
}

func TestRouteRejectsUnsupportedRecordType(t *testing.T) {
	router := &routerServer{}
	for _, value := range []string{`"BOGUS"`, `"ANY"`, `28`} {
		w := httptest.NewRecorder()
		router.routes().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/route",
			strings.NewReader(`{"domain":"app.example.com","record_type":`+value+`}`)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	}
}
