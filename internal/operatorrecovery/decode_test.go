package operatorrecovery

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const validConfirmationJSON = `{
	"generation": 7,
	"actor": "operator@example.test",
	"reason": "verified stopped processes and external writes",
	"helpersStopped": true,
	"externalWritesStopped": true,
	"sources": [{
		"sourceKey": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"board": "default",
		"kind": "publication",
		"sourceId": "pub_1",
		"observedUpdatedAt": "2026-07-24T01:02:03.000000000Z",
		"observedClaimEpoch": "3",
		"diagnosticCode": "publishing_ownership_unconfirmed",
		"disposition": "superseded",
		"outcome": "published",
		"resultUrl": "https://example.test/pull/3"
	}]
}`

func TestDecodeConfirmationStrictContract(t *testing.T) {
	value, err := DecodeConfirmation(strings.NewReader(validConfirmationJSON))
	if err != nil {
		t.Fatal(err)
	}
	if value.Generation != 7 || len(value.Sources) != 1 ||
		value.Sources[0].Outcome != PublicationOutcomePublished {
		t.Fatalf("decoded confirmation = %+v", value)
	}

	tests := []struct {
		name    string
		replace func(string) string
		want    string
	}{
		{
			name: "top level unknown",
			replace: func(value string) string {
				return strings.Replace(value, `"generation": 7`, `"claimToken": "secret", "generation": 7`, 1)
			},
			want: "unknown field",
		},
		{
			name: "nested unknown",
			replace: func(value string) string {
				return strings.Replace(value, `"sourceKey":`, `"claim_token": "secret", "sourceKey":`, 1)
			},
			want: "unknown field",
		},
		{
			name: "case alias rejected",
			replace: func(value string) string {
				return strings.Replace(value, `"sourceKey":`, `"SourceKey":`, 1)
			},
			want: "unknown field",
		},
		{
			name: "duplicate top level",
			replace: func(value string) string {
				return strings.Replace(value, `"generation": 7`, `"generation": 7, "generation": 8`, 1)
			},
			want: "duplicate field",
		},
		{
			name: "duplicate nested",
			replace: func(value string) string {
				return strings.Replace(value, `"board": "default"`, `"board": "default", "board": "other"`, 1)
			},
			want: "duplicate field",
		},
		{
			name: "trailing object",
			replace: func(value string) string {
				return value + ` {}`
			},
			want: "trailing JSON",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeConfirmation(strings.NewReader(test.replace(validConfirmationJSON)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
			if !errors.Is(err, ErrInvalidConfirmation) {
				t.Fatalf("error %v does not wrap ErrInvalidConfirmation", err)
			}
		})
	}
}

func TestDecodeConfirmationRejectsInvalidUnicode(t *testing.T) {
	invalidUTF8 := []byte(validConfirmationJSON)
	actor := []byte(`operator@example.test`)
	index := bytes.Index(invalidUTF8, actor)
	if index < 0 {
		t.Fatal("actor fixture not found")
	}
	invalidUTF8[index] = 0xff
	for name, raw := range map[string][]byte{
		"invalid UTF-8": invalidUTF8,
		"high surrogate": []byte(strings.Replace(
			validConfirmationJSON,
			`operator@example.test`,
			`\uD800`,
			1,
		)),
		"low surrogate": []byte(strings.Replace(
			validConfirmationJSON,
			`operator@example.test`,
			`\uDC00`,
			1,
		)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeConfirmation(bytes.NewReader(raw))
			if err == nil || !errors.Is(err, ErrInvalidConfirmation) {
				t.Fatalf("invalid Unicode error = %v", err)
			}
		})
	}

	validReplacement := strings.Replace(
		validConfirmationJSON,
		`operator@example.test`,
		`operator\uFFFD@example.test`,
		1,
	)
	value, err := DecodeConfirmation(strings.NewReader(validReplacement))
	if err != nil {
		t.Fatalf("valid replacement rune was rejected: %v", err)
	}
	if value.Actor != "operator\uFFFD@example.test" {
		t.Fatalf("replacement rune actor = %q", value.Actor)
	}
}
