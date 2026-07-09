package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
)

// rsaTestKey returns a freshly generated 2048-bit RSA key. Generated
// once per test call — these are cheap, and avoiding package-level
// state keeps tests independent.
func rsaTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return k
}
