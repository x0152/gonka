package cosmos_test

import (
	"encoding/hex"
	"strings"
	"testing"

	"trainshard/internal/infrastructure/adapters/signing/cosmos"
)

const (
	alice = "1b3a2f4e5c6d7a8b9c0d1e2f3a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d"
	bob   = "6d5c4b3a29187f6e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f21"
)

func TestASignatureNamesTheAccountThatMadeIt(t *testing.T) {
	key, err := cosmos.FromHex(alice)
	if err != nil {
		t.Fatalf("got %v, want a key", err)
	}
	if !strings.HasPrefix(string(key.Address()), "gonka1") {
		t.Fatalf("got %q, want the address the chain knows this key by", key.Address())
	}

	payload := []byte("POST /shards/1/deploy")
	signed, err := cosmos.Recover(payload, key.Sign(payload))
	if err != nil || signed != key.Address() {
		t.Fatalf("got %q %v, want %q", signed, err, key.Address())
	}
}

func TestASignatureIsWorthNothingOnAnotherMessage(t *testing.T) {
	key, err := cosmos.FromHex(alice)
	if err != nil {
		t.Fatalf("got %v, want a key", err)
	}
	other, err := cosmos.FromHex(bob)
	if err != nil {
		t.Fatalf("got %v, want a key", err)
	}

	signature := key.Sign([]byte("POST /shards/1/deploy"))
	recovered, err := cosmos.Recover([]byte("POST /shards/1/abort"), signature)
	if err == nil && recovered == key.Address() {
		t.Fatal("a signature over one request must not stand for another")
	}
	if other.Address() == key.Address() {
		t.Fatal("two keys must not share an address")
	}
}

func TestNothingSignsWithSomethingThatIsNotAKey(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":      "",
		"too short":  hex.EncodeToString([]byte("short")),
		"not hex":    "zzzz",
		"32 zeroes":  strings.Repeat("0", 64),
		"64 bytes":   strings.Repeat("ab", 64),
		"odd length": "abc",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cosmos.FromHex(raw); err == nil {
				t.Fatalf("%q was taken as a key", raw)
			}
		})
	}
}
