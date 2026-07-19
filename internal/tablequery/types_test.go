package tablequery

import (
	"strings"
	"testing"
)

func TestValidateEnvelopeUsesClosedPageSizesAndGlobalSearchBounds(t *testing.T) {
	valid := TenantsInput{CommonInput: CommonInput{Page: 0, PageSize: 25, GlobalSearch: "ร้าน"}, Filters: TenantFilters{}}
	if !ValidateEnvelope(&valid) {
		t.Fatal("valid table query rejected")
	}
	for _, input := range []TenantsInput{
		{CommonInput: CommonInput{Page: -1, PageSize: 25}},
		{CommonInput: CommonInput{Page: 0, PageSize: 10}},
		{CommonInput: CommonInput{Page: 0, PageSize: 25, GlobalSearch: "x"}},
	} {
		if ValidateEnvelope(&input) {
			t.Fatalf("invalid table query accepted: %+v", input)
		}
	}
}

func TestValidateEnvelopeCountsGlobalSearchAsUnicodeCharacters(t *testing.T) {
	valid := TenantsInput{CommonInput: CommonInput{Page: 0, PageSize: 25, GlobalSearch: strings.Repeat("ร", 160)}}
	if !ValidateEnvelope(&valid) {
		t.Fatal("160 Thai characters should be accepted")
	}
	invalid := TenantsInput{CommonInput: CommonInput{Page: 0, PageSize: 25, GlobalSearch: strings.Repeat("ร", 161)}}
	if ValidateEnvelope(&invalid) {
		t.Fatal("161 Thai characters should be rejected")
	}
}

func TestNewPageMetaUsesExactTotalEvenForEmptyRequestedPage(t *testing.T) {
	page := NewPageMeta(9, 25, 51)
	if page.Page != 9 || page.Total != 51 || page.TotalPages != 3 {
		t.Fatalf("NewPageMeta() = %+v", page)
	}
}

func TestDateRangeRejectsReversedAndMalformedDates(t *testing.T) {
	if !validDateRange("2026-07-01", "2026-07-19") {
		t.Fatal("valid date range rejected")
	}
	if validDateRange("2026-07-19", "2026-07-01") || validDateRange("19/07/2026", "") {
		t.Fatal("invalid date range accepted")
	}
}
