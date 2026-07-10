package main

import "testing"

func TestNormalizePipedPassword(t *testing.T) {
	value, err := normalizePipedPassword([]byte("a-long-admin-password\r\n"))
	if err != nil {
		t.Fatalf("normalizePipedPassword() error = %v", err)
	}
	if string(value) != "a-long-admin-password" {
		t.Fatalf("normalizePipedPassword() = %q", value)
	}
}

func TestNormalizePipedPasswordRejectsMultipleLines(t *testing.T) {
	if _, err := normalizePipedPassword([]byte("first-line\nsecond-line\n")); err == nil {
		t.Fatal("expected multiple lines to be rejected")
	}
}
