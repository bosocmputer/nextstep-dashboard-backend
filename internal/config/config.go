package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

const production = "production"

type LookupFunc func(string) (string, bool)

type Config struct {
	Environment               string
	HTTPAddr                  string
	DatabaseURL               string
	PublicBaseURL             *url.URL
	AdminUsername             string
	AdminPasswordHash         string
	SessionHMACKey            []byte
	EncryptionMasterKey       []byte
	EncryptionKeyID           string
	LineLoginChannelID        string
	LineMessagingAccessToken  string
	SMLAllowedPrefixes        []netip.Prefix
	SMLAllowedHosts           []string
	DatabaseMaxConnections    int
	DatabaseMinConnections    int
	ReportWorkerConcurrency   int
	DeliveryWorkerConcurrency int
}

func Load(lookup LookupFunc) (Config, error) {
	environment := valueOrDefault(lookup, "APP_ENV", "development")
	httpAddr := valueOrDefault(lookup, "HTTP_ADDR", ":8080")
	adminUsername := valueOrDefault(lookup, "ADMIN_USERNAME", "superadmin")

	requiredNames := []string{
		"DATABASE_URL",
		"PUBLIC_BASE_URL",
		"ADMIN_PASSWORD_HASH",
		"SESSION_HMAC_KEY",
		"ENCRYPTION_MASTER_KEY",
		"ENCRYPTION_KEY_ID",
		"SML_ALLOWED_CIDRS",
	}
	if environment == production {
		requiredNames = append(requiredNames, "LINE_LOGIN_CHANNEL_ID", "LINE_MESSAGING_CHANNEL_ACCESS_TOKEN")
	}
	values := make(map[string]string, len(requiredNames))
	missing := make([]string, 0, len(requiredNames))
	for _, name := range requiredNames {
		value, ok := lookup(name)
		value = strings.TrimSpace(value)
		if !ok || value == "" {
			missing = append(missing, name)
			continue
		}
		values[name] = value
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return Config{}, fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}

	publicBaseURL, err := url.Parse(values["PUBLIC_BASE_URL"])
	if err != nil || publicBaseURL.Host == "" {
		return Config{}, errors.New("PUBLIC_BASE_URL must be an absolute URL")
	}
	if environment == production && publicBaseURL.Scheme != "https" {
		return Config{}, errors.New("PUBLIC_BASE_URL must use HTTPS in production")
	}

	if err := validateDatabaseURL(values["DATABASE_URL"], environment); err != nil {
		return Config{}, err
	}
	if !strings.HasPrefix(values["ADMIN_PASSWORD_HASH"], "$argon2id$") {
		return Config{}, errors.New("ADMIN_PASSWORD_HASH must be an Argon2id encoded hash")
	}
	if len(values["ENCRYPTION_KEY_ID"]) > 64 || strings.ContainsAny(values["ENCRYPTION_KEY_ID"], " \t\r\n") {
		return Config{}, errors.New("ENCRYPTION_KEY_ID must be a compact identifier of at most 64 characters")
	}
	lineLoginChannelID := strings.TrimSpace(values["LINE_LOGIN_CHANNEL_ID"])
	if lineLoginChannelID == "" {
		lineLoginChannelID, _ = lookup("LINE_LOGIN_CHANNEL_ID")
		lineLoginChannelID = strings.TrimSpace(lineLoginChannelID)
	}
	if lineLoginChannelID != "" && (len(lineLoginChannelID) > 32 || strings.Trim(lineLoginChannelID, "0123456789") != "") {
		return Config{}, errors.New("LINE_LOGIN_CHANNEL_ID must contain 1 to 32 digits")
	}
	lineMessagingAccessToken := strings.TrimSpace(values["LINE_MESSAGING_CHANNEL_ACCESS_TOKEN"])
	if lineMessagingAccessToken == "" {
		lineMessagingAccessToken, _ = lookup("LINE_MESSAGING_CHANNEL_ACCESS_TOKEN")
		lineMessagingAccessToken = strings.TrimSpace(lineMessagingAccessToken)
	}
	if lineMessagingAccessToken != "" && (len(lineMessagingAccessToken) < 32 || len(lineMessagingAccessToken) > 4096 || strings.ContainsAny(lineMessagingAccessToken, " \t\r\n")) {
		return Config{}, errors.New("LINE_MESSAGING_CHANNEL_ACCESS_TOKEN must be a compact token containing 32 to 4096 characters")
	}
	allowedPrefixes, err := parseAllowedPrefixes(values["SML_ALLOWED_CIDRS"])
	if err != nil {
		return Config{}, err
	}
	allowedHosts, err := parseAllowedHosts(valueOrDefault(lookup, "SML_ALLOWED_HOSTS", ""))
	if err != nil {
		return Config{}, err
	}
	workerConcurrency, err := intValue(lookup, "REPORT_WORKER_CONCURRENCY", 4, 1, 16)
	if err != nil {
		return Config{}, err
	}
	deliveryConcurrency, err := intValue(lookup, "DELIVERY_WORKER_CONCURRENCY", 4, 1, 16)
	if err != nil {
		return Config{}, err
	}
	databaseMaxConnections, err := intValue(lookup, "DATABASE_MAX_CONNECTIONS", 20, 2, 200)
	if err != nil {
		return Config{}, err
	}
	databaseMinConnections, err := intValue(lookup, "DATABASE_MIN_CONNECTIONS", 2, 0, 50)
	if err != nil {
		return Config{}, err
	}
	if databaseMinConnections > databaseMaxConnections {
		return Config{}, errors.New("DATABASE_MIN_CONNECTIONS must not exceed DATABASE_MAX_CONNECTIONS")
	}

	sessionKey, err := decodeKey("SESSION_HMAC_KEY", values["SESSION_HMAC_KEY"], 32, false)
	if err != nil {
		return Config{}, err
	}
	encryptionKey, err := decodeKey("ENCRYPTION_MASTER_KEY", values["ENCRYPTION_MASTER_KEY"], 32, true)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Environment:               environment,
		HTTPAddr:                  httpAddr,
		DatabaseURL:               values["DATABASE_URL"],
		PublicBaseURL:             publicBaseURL,
		AdminUsername:             adminUsername,
		AdminPasswordHash:         values["ADMIN_PASSWORD_HASH"],
		SessionHMACKey:            sessionKey,
		EncryptionMasterKey:       encryptionKey,
		EncryptionKeyID:           values["ENCRYPTION_KEY_ID"],
		LineLoginChannelID:        lineLoginChannelID,
		LineMessagingAccessToken:  lineMessagingAccessToken,
		SMLAllowedPrefixes:        allowedPrefixes,
		SMLAllowedHosts:           allowedHosts,
		DatabaseMaxConnections:    databaseMaxConnections,
		DatabaseMinConnections:    databaseMinConnections,
		ReportWorkerConcurrency:   workerConcurrency,
		DeliveryWorkerConcurrency: deliveryConcurrency,
	}, nil
}

func parseAllowedHosts(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	seen := make(map[string]struct{})
	hosts := make([]string, 0)
	for _, part := range strings.Split(raw, ",") {
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(part), "."))
		if !validAllowedHostname(host) {
			return nil, errors.New("SML_ALLOWED_HOSTS must contain exact ASCII hostnames")
		}
		if _, exists := seen[host]; !exists {
			seen[host] = struct{}{}
			hosts = append(hosts, host)
		}
	}
	slices.Sort(hosts)
	return hosts, nil
}

func validAllowedHostname(host string) bool {
	if len(host) < 1 || len(host) > 253 || !strings.Contains(host, ".") || strings.Contains(host, "..") {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func intValue(lookup LookupFunc, name string, fallback, minimum, maximum int) (int, error) {
	raw := valueOrDefault(lookup, name, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

func parseAllowedPrefixes(raw string) ([]netip.Prefix, error) {
	parts := strings.Split(raw, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err != nil {
			return nil, errors.New("SML_ALLOWED_CIDRS must contain valid CIDR prefixes")
		}
		prefix = prefix.Masked()
		if (prefix.Addr().Is4() && prefix.Bits() < 8) || (prefix.Addr().Is6() && prefix.Bits() < 32) {
			return nil, errors.New("SML_ALLOWED_CIDRS contains an excessively broad prefix")
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func valueOrDefault(lookup LookupFunc, name, fallback string) string {
	value, ok := lookup(name)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func validateDatabaseURL(rawURL, environment string) error {
	databaseURL, err := url.Parse(rawURL)
	if err != nil || databaseURL.Host == "" {
		return errors.New("DATABASE_URL must be an absolute PostgreSQL URL")
	}
	if databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql" {
		return errors.New("DATABASE_URL must use postgres or postgresql scheme")
	}
	if environment != production {
		return nil
	}
	hostname := strings.ToLower(databaseURL.Hostname())
	if hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1" {
		return errors.New("DATABASE_URL must not use a loopback host in production")
	}
	sslMode := strings.ToLower(databaseURL.Query().Get("sslmode"))
	if sslMode == "" || sslMode == "disable" || sslMode == "allow" || sslMode == "prefer" {
		return errors.New("DATABASE_URL must require verified TLS in production")
	}
	return nil
}

func decodeKey(name, encoded string, minimumLength int, exact bool) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%s must be valid base64", name)
	}
	if exact && len(decoded) != minimumLength {
		return nil, fmt.Errorf("%s must decode to exactly %d bytes", name, minimumLength)
	}
	if !exact && len(decoded) < minimumLength {
		return nil, fmt.Errorf("%s must decode to at least %d bytes", name, minimumLength)
	}
	return decoded, nil
}
