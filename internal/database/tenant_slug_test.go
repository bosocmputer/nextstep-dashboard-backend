package database

import (
	"bytes"
	"regexp"
	"testing"
)

func TestGenerateTenantSlugUsesStableInternalFormat(t *testing.T) {
	slug, err := generateTenantSlug(bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8}))
	if err != nil {
		t.Fatalf("generateTenantSlug() error = %v", err)
	}
	if !regexp.MustCompile(`^shop-[0-9a-hjkmnp-tv-z]{12}$`).MatchString(slug) {
		t.Fatalf("slug = %q", slug)
	}
}
