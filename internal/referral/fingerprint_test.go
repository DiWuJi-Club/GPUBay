package referral_test

import (
	"bytes"
	"testing"

	"spark-cloud/internal/referral"
)

func TestFingerprintDeterministicAndKeyed(t *testing.T) {
	k1, err := referral.DeriveFingerprintKey(bytes.Repeat([]byte("a"), 32))
	if err != nil || len(k1) != 32 {
		t.Fatal(err)
	}
	k2, _ := referral.DeriveFingerprintKey(bytes.Repeat([]byte("b"), 32))
	a := referral.Fingerprint(k1, "kyc", "110101199001011234")
	if a != referral.Fingerprint(k1, "kyc", "110101199001011234") || len(a) != 64 {
		t.Fatalf("not deterministic / wrong length: %q", a)
	}
	if a == referral.Fingerprint(k2, "kyc", "110101199001011234") {
		t.Fatal("different keys must differ")
	}
	if a == referral.Fingerprint(k1, "lic", "110101199001011234") || a == referral.Fingerprint(k1, "kyc", "110101199001011235") {
		t.Fatal("kind / number must be part of the input")
	}
	if _, err := referral.DeriveFingerprintKey(nil); err == nil {
		t.Fatal("empty secret must fail")
	}
}
