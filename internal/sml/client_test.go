package sml

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestDecompressPayloadClassifiesSafeZIPFailures(t *testing.T) {
	emptyBuffer := new(bytes.Buffer)
	emptyWriter := zip.NewWriter(emptyBuffer)
	if err := emptyWriter.Close(); err != nil {
		t.Fatal(err)
	}

	tooLarge, err := CompressPayload([]byte("12345"))
	if err != nil {
		t.Fatal(err)
	}

	corruptBuffer := new(bytes.Buffer)
	corruptWriter := zip.NewWriter(corruptBuffer)
	header := &zip.FileHeader{Name: "0", Method: zip.Store}
	entry, err := corruptWriter.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := corruptWriter.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), corruptBuffer.Bytes()...)
	dataAt := bytes.Index(corrupt, []byte("hello"))
	if dataAt < 0 {
		t.Fatal("stored ZIP payload did not contain test data")
	}
	corrupt[dataAt] ^= 0xff

	tests := []struct {
		name    string
		payload []byte
		limit   int64
		want    error
		code    string
	}{
		{name: "invalid format", payload: []byte("not-a-zip"), limit: 1024, want: ErrZIPFormatInvalid, code: "SML_ZIP_FORMAT_INVALID"},
		{name: "empty archive", payload: emptyBuffer.Bytes(), limit: 1024, want: ErrZIPEmpty, code: "SML_ZIP_EMPTY"},
		{name: "uncompressed too large", payload: tooLarge, limit: 4, want: ErrZIPTooLarge, code: "SML_ZIP_TOO_LARGE"},
		{name: "corrupt entry", payload: corrupt, limit: 1024, want: ErrZIPReadFailed, code: "SML_ZIP_READ_FAILED"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecompressPayload(test.payload, test.limit)
			if !errors.Is(err, test.want) {
				t.Fatalf("DecompressPayload() error = %v, want %v", err, test.want)
			}
			if got := zipSafeErrorCode(err); got != test.code {
				t.Fatalf("zipSafeErrorCode() = %q, want %q", got, test.code)
			}
		})
	}
}

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

func TestCanonicalHostKeyNormalizesDefaultPortsAndIgnoresJavaWSPath(t *testing.T) {
	httpsDefault, err := CanonicalHostKey("https://SML-Shop.Example.com/SMLJavaWebService/DotNetFrameWork")
	if err != nil {
		t.Fatal(err)
	}
	httpsExplicit, err := CanonicalHostKey("https://sml-shop.example.com:443/other")
	if err != nil {
		t.Fatal(err)
	}
	httpKey, err := CanonicalHostKey("http://sml-shop.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if httpsDefault != httpsExplicit {
		t.Fatal("equivalent HTTPS origins produced different host keys")
	}
	if httpsDefault == httpKey {
		t.Fatal("HTTP and HTTPS origins must not share a host circuit")
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

func TestEndpointPolicyAllowsPublicEndpointsWithoutPerTenantAllowlist(t *testing.T) {
	policy := EndpointPolicy{
		AllowPublicEndpoints: true,
		AllowedPorts:         []uint16{80, 443, 8080, 8092},
		LookupNetIP: func(context.Context, string, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("113.53.47.214")}, nil
		},
	}

	resolved, err := policy.Resolve(context.Background(), "http://cspromart.thaiddns.com:8080")
	if err != nil {
		t.Fatalf("Resolve() public customer hostname error = %v", err)
	}
	if got := resolved.URL.String(); got != "http://cspromart.thaiddns.com:8080/SMLJavaWebService/DotNetFrameWork" {
		t.Fatalf("resolved URL = %q", got)
	}

	resolvedIP, err := policy.Resolve(context.Background(), "http://103.76.180.199:8080")
	if err != nil {
		t.Fatalf("Resolve() public customer IP error = %v", err)
	}
	if got := resolvedIP.URL.String(); got != "http://103.76.180.199:8080/SMLJavaWebService/DotNetFrameWork" {
		t.Fatalf("resolved public IP URL = %q", got)
	}
	if _, err := policy.Resolve(context.Background(), "http://cspromart.thaiddns.com:9000"); err == nil {
		t.Fatal("unapproved public endpoint port was accepted")
	}

	policy.LookupNetIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.121.19.150")}, nil
	}
	if _, err := policy.Resolve(context.Background(), "http://cspromart.thaiddns.com:8080"); err == nil {
		t.Fatal("public hostname mode accepted a private DNS answer without an allowed CIDR")
	}
	if _, err := policy.Resolve(context.Background(), "http://10.121.19.150:8080"); err == nil {
		t.Fatal("public endpoint mode accepted a private address without an allowed CIDR")
	}
	if _, err := policy.Resolve(context.Background(), "http://169.254.169.254:8080"); err == nil {
		t.Fatal("public endpoint mode accepted the cloud metadata address")
	}
}

func TestEndpointPolicyAllowsAnyPortForSafePublicDNSWhenPortRestrictionIsEmpty(t *testing.T) {
	policy := EndpointPolicy{
		AllowPublicEndpoints: true,
		LookupNetIP: func(context.Context, string, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("49.228.131.14")}, nil
		},
	}

	resolved, err := policy.Resolve(context.Background(), "http://cspromart.thddns.com:2210")
	if err != nil {
		t.Fatalf("Resolve() arbitrary public port error = %v", err)
	}
	if got := resolved.URL.String(); got != "http://cspromart.thddns.com:2210/SMLJavaWebService/DotNetFrameWork" {
		t.Fatalf("resolved URL = %q", got)
	}

	policy.LookupNetIP = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	if _, err := policy.Resolve(context.Background(), "http://cspromart.thddns.com:2210"); err == nil {
		t.Fatal("unrestricted port mode bypassed the metadata address block")
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
