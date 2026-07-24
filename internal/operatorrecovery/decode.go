package operatorrecovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

var confirmationKeys = map[string]struct{}{
	"generation":            {},
	"actor":                 {},
	"reason":                {},
	"helpersStopped":        {},
	"externalWritesStopped": {},
	"sources":               {},
}

var confirmationSourceKeys = map[string]struct{}{
	"sourceKey":          {},
	"board":              {},
	"kind":               {},
	"sourceId":           {},
	"observedUpdatedAt":  {},
	"observedClaimEpoch": {},
	"diagnosticCode":     {},
	"disposition":        {},
	"outcome":            {},
	"resultUrl":          {},
}

// DecodeConfirmation decodes exactly one confirmation object. Callers own
// transport size limits (for example, io.LimitReader at the CLI/API boundary).
// This function rejects unknown fields, duplicate keys at every object level,
// key casing aliases, and trailing JSON.
func DecodeConfirmation(reader io.Reader) (Confirmation, error) {
	value, err := decodeConfirmation(reader)
	if err == nil {
		return value, nil
	}
	if errors.Is(err, ErrInvalidConfirmation) {
		return Confirmation{}, err
	}
	return Confirmation{}, fmt.Errorf("%w: %v", ErrInvalidConfirmation, err)
}

func decodeConfirmation(reader io.Reader) (Confirmation, error) {
	if reader == nil {
		return Confirmation{}, errors.New("operator recovery confirmation is required")
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return Confirmation{}, fmt.Errorf("read operator recovery confirmation: %w", err)
	}
	if !utf8.Valid(raw) {
		return Confirmation{}, errors.New(
			"operator recovery confirmation must be valid UTF-8",
		)
	}
	if err := validateJSONStringUnicode(raw); err != nil {
		return Confirmation{}, err
	}
	if err := validateConfirmationJSON(raw); err != nil {
		return Confirmation{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value Confirmation
	if err := decoder.Decode(&value); err != nil {
		return Confirmation{}, fmt.Errorf("decode operator recovery confirmation: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Confirmation{}, err
	}
	return value, nil
}

func validateJSONStringUnicode(raw []byte) error {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '"' {
			continue
		}
		index++
		closed := false
		for index < len(raw) {
			value := raw[index]
			switch {
			case value == '"':
				closed = true
			case value < 0x20:
				return errors.New(
					"operator recovery JSON string contains a control character",
				)
			case value != '\\':
				// Raw UTF-8 was validated for the whole document above.
			default:
				index++
				if index >= len(raw) {
					return errors.New(
						"operator recovery JSON string has an incomplete escape",
					)
				}
				escape := raw[index]
				if escape != 'u' {
					if !strings.ContainsRune(`"\/bfnrt`, rune(escape)) {
						return errors.New(
							"operator recovery JSON string has an invalid escape",
						)
					}
					break
				}
				codePoint, next, err := jsonUnicodeEscape(raw, index)
				if err != nil {
					return err
				}
				index = next
				switch {
				case codePoint >= 0xd800 && codePoint <= 0xdbff:
					if index+2 >= len(raw) ||
						raw[index+1] != '\\' ||
						raw[index+2] != 'u' {
						return errors.New(
							"operator recovery JSON contains an unpaired high surrogate",
						)
					}
					low, lowNext, err := jsonUnicodeEscape(raw, index+2)
					if err != nil {
						return err
					}
					if low < 0xdc00 || low > 0xdfff {
						return errors.New(
							"operator recovery JSON contains an unpaired high surrogate",
						)
					}
					index = lowNext
				case codePoint >= 0xdc00 && codePoint <= 0xdfff:
					return errors.New(
						"operator recovery JSON contains an unpaired low surrogate",
					)
				}
			}
			if closed {
				break
			}
			index++
		}
		if !closed {
			return errors.New(
				"operator recovery JSON contains an unterminated string",
			)
		}
	}
	return nil
}

func jsonUnicodeEscape(
	raw []byte,
	uIndex int,
) (uint16, int, error) {
	if uIndex+4 >= len(raw) {
		return 0, uIndex, errors.New(
			"operator recovery JSON has an incomplete Unicode escape",
		)
	}
	var value uint16
	for offset := 1; offset <= 4; offset++ {
		digit := raw[uIndex+offset]
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, uIndex, errors.New(
				"operator recovery JSON has an invalid Unicode escape",
			)
		}
	}
	return value, uIndex + 4, nil
}

func validateConfirmationJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode operator recovery confirmation: %w", err)
	}
	if token != json.Delim('{') {
		return errors.New("operator recovery confirmation must be a JSON object")
	}
	if err := validateObject(
		decoder,
		"confirmation",
		confirmationKeys,
		func(key string) error {
			if key != "sources" {
				return consumeJSONValue(decoder, "confirmation."+key)
			}
			return validateSources(decoder)
		},
	); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	return nil
}

func validateSources(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode confirmation.sources: %w", err)
	}
	if token != json.Delim('[') {
		return errors.New("confirmation.sources must be an array")
	}
	index := 0
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode confirmation.sources: %w", err)
		}
		if token != json.Delim('{') {
			return fmt.Errorf("confirmation.sources[%d] must be an object", index)
		}
		path := fmt.Sprintf("confirmation.sources[%d]", index)
		if err := validateObject(
			decoder,
			path,
			confirmationSourceKeys,
			func(key string) error {
				return consumeJSONValue(decoder, path+"."+key)
			},
		); err != nil {
			return err
		}
		index++
	}
	token, err = decoder.Token()
	if err != nil {
		return fmt.Errorf("decode confirmation.sources: %w", err)
	}
	if token != json.Delim(']') {
		return errors.New("confirmation.sources has an invalid closing token")
	}
	return nil
}

func validateObject(
	decoder *json.Decoder,
	path string,
	allowed map[string]struct{},
	consume func(string) error,
) error {
	seen := make(map[string]struct{}, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("%s contains a non-string object key", path)
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("%s contains unknown field %q", path, key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%s contains duplicate field %q", path, key)
		}
		seen[key] = struct{}{}
		if err := consume(key); err != nil {
			return err
		}
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if token != json.Delim('}') {
		return fmt.Errorf("%s has an invalid closing token", path)
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder, path+"[]"); err != nil {
				return err
			}
		}
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		if token != json.Delim(']') {
			return fmt.Errorf("%s has an invalid array closing token", path)
		}
		return nil
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("decode %s: %w", path, err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s contains a non-string object key", path)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%s contains duplicate field %q", path, key)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		if token != json.Delim('}') {
			return fmt.Errorf("%s has an invalid object closing token", path)
		}
		return nil
	default:
		return fmt.Errorf("%s starts with unexpected JSON delimiter %q", path, delim)
	}
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing operator recovery JSON: %w", err)
	}
	return errors.New("operator recovery confirmation contains trailing JSON")
}
