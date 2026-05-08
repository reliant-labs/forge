//go:build ignore

package middleware

import (
	"testing"
)

func TestRedactFields_NilInput(t *testing.T) {
	t.Parallel()
	if got := RedactFields(nil, "x"); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestRedactFields_NonStruct(t *testing.T) {
	t.Parallel()
	got := RedactFields("hello", "x")
	if got["value"] != "hello" {
		t.Fatalf("expected wrapped value, got %v", got)
	}
}

func TestRedactFields_RedactsNamedFields(t *testing.T) {
	t.Parallel()
	type User struct {
		Name     string
		Email    string
		Password string
		SSN      string
	}
	got := RedactFields(User{
		Name:     "Alice",
		Email:    "alice@example.com",
		Password: "s3cret",
		SSN:      "123-45-6789",
	}, "password", "ssn")

	if got["Name"] != "Alice" {
		t.Fatalf("Name should not be redacted, got %v", got["Name"])
	}
	if got["Email"] != "alice@example.com" {
		t.Fatalf("Email should not be redacted, got %v", got["Email"])
	}
	if got["Password"] != "[REDACTED]" {
		t.Fatalf("Password should be redacted, got %v", got["Password"])
	}
	if got["SSN"] != "[REDACTED]" {
		t.Fatalf("SSN should be redacted, got %v", got["SSN"])
	}
}

func TestRedactFields_PointerInput(t *testing.T) {
	t.Parallel()
	type Data struct {
		Token string
		Value int
	}
	got := RedactFields(&Data{Token: "abc", Value: 42}, "token")
	if got["Token"] != "[REDACTED]" {
		t.Fatalf("Token should be redacted, got %v", got["Token"])
	}
	if got["Value"] != 42 {
		t.Fatalf("Value should be 42, got %v", got["Value"])
	}
}

func TestRedactFields_CaseInsensitive(t *testing.T) {
	t.Parallel()
	type Data struct {
		APIKey string
	}
	got := RedactFields(Data{APIKey: "key123"}, "apikey")
	if got["APIKey"] != "[REDACTED]" {
		t.Fatalf("APIKey should be redacted case-insensitively, got %v", got["APIKey"])
	}
}

func TestRedactFields_UnknownFieldIgnored(t *testing.T) {
	t.Parallel()
	type Data struct {
		Name string
	}
	got := RedactFields(Data{Name: "Bob"}, "nonexistent")
	if got["Name"] != "Bob" {
		t.Fatalf("Name should remain, got %v", got["Name"])
	}
}
