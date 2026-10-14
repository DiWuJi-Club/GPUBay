package secretbox_test

import (
	"bytes"
	"testing"

	"spark-cloud/internal/platform/secretbox"
)

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := secretbox.New(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed := box.Seal([]byte("110101199001011234"))
	if bytes.Contains(sealed, []byte("1101011990")) {
		t.Fatal("ciphertext contains plaintext")
	}
	plain, err := box.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "110101199001011234" {
		t.Fatalf("got %q", plain)
	}
}

func TestOpenRejectsTamperedData(t *testing.T) {
	box, _ := secretbox.New(bytes.Repeat([]byte("k"), 32))
	sealed := box.Seal([]byte("secret"))
	sealed[len(sealed)-1] ^= 0xff
	if _, err := box.Open(sealed); err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}

func TestNewRejectsWrongKeyLength(t *testing.T) {
	if _, err := secretbox.New([]byte("short")); err == nil {
		t.Fatal("expected error for short key")
	}
}
