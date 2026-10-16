package admin

import "testing"

func TestHashRecoveryCodeNormalizes(t *testing.T) {
	if hashRecoveryCode(" ABCDE-12345 ") != hashRecoveryCode("abcde12345") {
		t.Fatal("recovery code hash must ignore case, spaces and hyphens")
	}
	if hashRecoveryCode("abcde-12345") == hashRecoveryCode("abcde-12346") {
		t.Fatal("different codes must hash differently")
	}
}
