package sml

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestCompressedPayloadAndSOAPRowsRoundTrip(t *testing.T) {
	compressed, err := CompressPayload([]byte("select 1 as ok"))
	if err != nil {
		t.Fatalf("CompressPayload() error = %v", err)
	}
	decompressed, err := DecompressPayload(compressed, 1024)
	if err != nil || string(decompressed) != "select 1 as ok" {
		t.Fatalf("DecompressPayload() = %q, %v", decompressed, err)
	}

	resultXML := []byte(`<?xml version="1.0"?><ResultSet><Row><doc_no>IV-001</doc_no><total_amount>123.45</total_amount></Row></ResultSet>`)
	zippedResult, _ := CompressPayload(resultXML)
	soap := BuildQueryEnvelope("NEXTSTEP", "SMLConfigDATA.xml", "sml1_2026", base64.StdEncoding.EncodeToString(zippedResult))
	if !strings.Contains(soap, `<_queryCompress xmlns="http://SMLWebService/">`) || !strings.Contains(soap, `<arg2 xmlns="">sml1_2026</arg2>`) {
		t.Fatalf("unexpected SOAP envelope: %s", soap)
	}
	response := `<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><ns2:_queryCompressResponse xmlns:ns2="http://SMLWebService/"><return>` + base64.StdEncoding.EncodeToString(zippedResult) + `</return></ns2:_queryCompressResponse></soap:Body></soap:Envelope>`
	encoded, err := ExtractSOAPReturn([]byte(response))
	if err != nil {
		t.Fatalf("ExtractSOAPReturn() error = %v", err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(encoded)
	xmlPayload, _ := DecompressPayload(decoded, 4096)
	rows, err := ParseRows(xmlPayload, 10)
	if err != nil || len(rows) != 1 || rows[0]["doc_no"] != "IV-001" || rows[0]["total_amount"] != "123.45" {
		t.Fatalf("ParseRows() = %+v, %v", rows, err)
	}
}

func TestEndpointPolicyRequiresEveryResolvedAddressInAllowlist(t *testing.T) {
	policy := EndpointPolicy{
		AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		LookupNetIP: func(context.Context, string, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.0.0.8"), net.ParseIP("203.0.113.8")}, nil
		},
	}
	if _, err := policy.Resolve(context.Background(), "http://sml.example.internal/SMLJavaWebService/DotNetFrameWork"); err == nil {
		t.Fatal("mixed allowed/public DNS answer was accepted")
	}
	policy.LookupNetIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	policy.AllowedPrefixes = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	if _, err := policy.Resolve(context.Background(), "http://metadata.internal/service"); err == nil {
		t.Fatal("cloud metadata address was accepted even with broad allowlist")
	}
}

func TestEndpointPolicyAllowsExactHostnameAndNormalizesJavaWSPath(t *testing.T) {
	policy := EndpointPolicy{
		AllowedHosts: []string{"sml-shop.example.com"},
		LookupNetIP: func(context.Context, string, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("58.136.190.202")}, nil
		},
	}
	resolved, err := policy.Resolve(context.Background(), "http://sml-shop.example.com:8092")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := resolved.URL.String(); got != "http://sml-shop.example.com:8092/SMLJavaWebService/DotNetFrameWork" {
		t.Fatalf("resolved URL = %q", got)
	}

	policy.LookupNetIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	if _, err := policy.Resolve(context.Background(), "http://sml-shop.example.com:8092"); err == nil {
		t.Fatal("hostname allowlist bypassed the metadata address block")
	}
	policy.LookupNetIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	if _, err := policy.Resolve(context.Background(), "http://sml-shop.example.com:8092"); err == nil {
		t.Fatal("hostname allowlist accepted a loopback DNS answer")
	}
}

func TestClientQueriesPinnedEndpointWithBoundedResponse(t *testing.T) {
	resultXML := []byte(`<ResultSet><Row><ok>true</ok></Row></ResultSet>`)
	zippedResult, _ := CompressPayload(resultXML)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "sml-user" || password != "sml-password" {
			t.Fatalf("unexpected basic auth: %q %q %v", username, password, ok)
		}
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "text/xml; charset=utf-8" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.Header.Get("Content-Type"))
		}
		response.Header().Set("Content-Type", "text/xml")
		_, _ = response.Write([]byte(`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body><response><return>` + base64.StdEncoding.EncodeToString(zippedResult) + `</return></response></soap:Body></soap:Envelope>`))
	}))
	defer server.Close()

	policy := EndpointPolicy{AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	client := NewClient(policy, 2*time.Second, 1024*1024, 100)
	rows, err := client.Query(context.Background(), Connection{
		EndpointURL:    server.URL,
		ConfigFileName: "SMLConfigDATA.xml",
		DatabaseName:   "demo",
		Username:       "sml-user",
		Password:       "sml-password",
	}, "select true as ok")
	if err != nil || len(rows) != 1 || rows[0]["ok"] != "true" {
		t.Fatalf("Query() = %+v, %v", rows, err)
	}
}

func TestParseRowsRejectsRowLimitAndMalformedResponse(t *testing.T) {
	if _, err := ParseRows([]byte(`<ResultSet><Row><id>1</id></Row><Row><id>2</id></Row></ResultSet>`), 1); err == nil {
		t.Fatal("row limit was not enforced")
	}
	if _, err := ExtractSOAPReturn([]byte(`<soap><Fault><faultstring>database password leaked</faultstring></Fault></soap>`)); err == nil || strings.Contains(err.Error(), "password leaked") {
		t.Fatalf("SOAP fault was not safely redacted: %v", err)
	}
}
