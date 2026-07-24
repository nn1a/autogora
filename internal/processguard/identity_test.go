package processguard

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func testDurableReceiptPrivateKey() ed25519.PrivateKey {
	seed := bytes.Repeat([]byte{0x5a}, ed25519.SeedSize)
	return ed25519.NewKeyFromSeed(seed)
}

func validTestDurableIdentity() DurableIdentity {
	privateKey := testDurableReceiptPrivateKey()
	return DurableIdentity{
		Version:            DurableIdentityVersion,
		BootID:             "12345678-1234-1234-1234-123456789abc",
		PIDNamespaceDevice: 4,
		PIDNamespaceInode:  4026531836,
		GuardPID:           1234,
		StartTimeTicks:     5678,
		ProcessGroupID:     1234,
		ExecutionID:        strings.Repeat("12", durableIdentifierBytes),
		ReceiptID:          strings.Repeat("34", durableIdentifierBytes),
		ReceiptPublicKey: hex.EncodeToString(
			privateKey.Public().(ed25519.PublicKey),
		),
	}
}

func signedTestDurableReceipt(
	t *testing.T,
	identity DurableIdentity,
	released bool,
) DurableTeardownReceipt {
	t.Helper()
	receipt, err := signDurableTeardownReceipt(
		DurableTeardownReceipt{
			Version:   DurableTeardownReceiptVersion,
			Identity:  identity,
			Released:  released,
			Quiescent: true,
		},
		testDurableReceiptPrivateKey(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestDurableIdentityValidateRejectsIncompleteOrAmbiguousIdentity(
	t *testing.T,
) {
	valid := validTestDurableIdentity()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid identity: %v", err)
	}

	tests := map[string]func(*DurableIdentity){
		"version": func(identity *DurableIdentity) {
			identity.Version++
		},
		"boot ID": func(identity *DurableIdentity) {
			identity.BootID = strings.ToUpper(identity.BootID)
		},
		"namespace device": func(identity *DurableIdentity) {
			identity.PIDNamespaceDevice = 0
		},
		"namespace inode": func(identity *DurableIdentity) {
			identity.PIDNamespaceInode = 0
		},
		"guard PID": func(identity *DurableIdentity) {
			identity.GuardPID = 0
		},
		"start ticks": func(identity *DurableIdentity) {
			identity.StartTimeTicks = 0
		},
		"process group": func(identity *DurableIdentity) {
			identity.ProcessGroupID++
		},
		"execution ID": func(identity *DurableIdentity) {
			identity.ExecutionID = strings.Repeat("0", durableIdentifierBytes*2)
		},
		"receipt ID": func(identity *DurableIdentity) {
			identity.ReceiptID = identity.ExecutionID
		},
		"receipt public key": func(identity *DurableIdentity) {
			identity.ReceiptPublicKey = strings.ToUpper(
				identity.ReceiptPublicKey,
			)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			identity := valid
			mutate(&identity)
			if err := identity.Validate(); err == nil {
				t.Fatal("invalid identity was accepted")
			}
		})
	}
}

func TestDurableTeardownReceiptRequiresExactCanonicalSchema(t *testing.T) {
	receipt := signedTestDurableReceipt(
		t,
		validTestDurableIdentity(),
		true,
	)
	canonical, err := receipt.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDurableTeardownReceipt(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != receipt {
		t.Fatalf("parsed receipt = %#v, want %#v", parsed, receipt)
	}

	nonCanonical := [][]byte{
		append(append([]byte(nil), canonical...), '\n'),
		bytes.Replace(
			canonical,
			[]byte(`"version":2`),
			[]byte(`"version":2,"unknown":true`),
			1,
		),
		bytes.Replace(
			canonical,
			[]byte(`"released":true`),
			[]byte(`"released":true,"released":true`),
			1,
		),
	}
	for _, raw := range nonCanonical {
		if _, err := ParseDurableTeardownReceipt(raw); err == nil {
			t.Fatalf("non-canonical receipt was accepted: %s", raw)
		}
	}
}

func TestDurableTeardownReceiptRejectsNegativeAttestation(t *testing.T) {
	receipt := DurableTeardownReceipt{
		Version:   DurableTeardownReceiptVersion,
		Identity:  validTestDurableIdentity(),
		Released:  false,
		Quiescent: false,
	}
	if _, err := receipt.CanonicalJSON(); err == nil {
		t.Fatal("non-quiescent receipt was accepted")
	}
}

func TestDurableTeardownReceiptRequiresExpectedIdentity(t *testing.T) {
	expected := validTestDurableIdentity()
	receipt := signedTestDurableReceipt(t, expected, true)
	raw, err := receipt.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDurableTeardownReceiptForIdentity(
		raw,
		expected,
	); err != nil {
		t.Fatal(err)
	}
	different := expected
	different.ReceiptID = strings.Repeat("56", durableIdentifierBytes)
	if _, err := ParseDurableTeardownReceiptForIdentity(
		raw,
		different,
	); !errors.Is(err, ErrDurableReceiptIdentityMismatch) {
		t.Fatalf("receipt identity mismatch error = %v", err)
	}
}

func TestDurableTeardownReceiptRejectsSignatureTampering(t *testing.T) {
	receipt := signedTestDurableReceipt(
		t,
		validTestDurableIdentity(),
		true,
	)
	raw, err := receipt.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(
		raw,
		[]byte(`"released":true`),
		[]byte(`"released":false`),
		1,
	)
	if _, err := ParseDurableTeardownReceipt(tampered); !errors.Is(
		err,
		ErrDurableReceiptSignatureInvalid,
	) {
		t.Fatalf("tampered receipt error = %v", err)
	}

	receipt.Signature = strings.Repeat(
		"00",
		durableReceiptSignatureBytes,
	)
	if _, err := receipt.CanonicalJSON(); err == nil {
		t.Fatal("all-zero receipt signature was accepted")
	}
}
