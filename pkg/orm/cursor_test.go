package orm

import (
	"testing"
)

func TestEncodeDecode_RoundTrip(t *testing.T) {
	ids := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"simple-id",
		"123",
		"abc/def+ghi=jkl",
	}

	for _, id := range ids {
		token := EncodeCursor(id)
		if token == "" {
			t.Errorf("EncodeCursor(%q) returned empty string", id)
		}

		got, err := DecodeCursor(token)
		if err != nil {
			t.Errorf("DecodeCursor(EncodeCursor(%q)) error: %v", id, err)
		}
		if got != id {
			t.Errorf("DecodeCursor(EncodeCursor(%q)) = %q, want %q", id, got, id)
		}
	}
}

func TestDecodeCursor_EmptyToken(t *testing.T) {
	_, err := DecodeCursor("")
	if err == nil {
		t.Error("DecodeCursor(\"\") should return error")
	}
}

func TestDecodeCursor_InvalidToken(t *testing.T) {
	_, err := DecodeCursor("!!!invalid-base64!!!")
	if err == nil {
		t.Error("DecodeCursor with invalid base64 should return error")
	}
}

func TestEncodeCursor_EmptyID(t *testing.T) {
	token := EncodeCursor("")
	// Empty string encodes to empty base64, which DecodeCursor rejects.
	if token != "" {
		t.Errorf("EncodeCursor(\"\") = %q, want empty", token)
	}
}