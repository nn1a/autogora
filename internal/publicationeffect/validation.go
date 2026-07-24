package publicationeffect

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxRefBytes   = 1024
	maxTitleBytes = 512
)

type controlPolicy uint8

const (
	controlNone controlPolicy = iota
	controlTextLines
)

func validateText(
	value string,
	field string,
	maxBytes int,
	policy controlPolicy,
) error {
	if value == "" {
		return fmt.Errorf("%s cannot be empty", field)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s must be at most %d bytes", field, maxBytes)
	}
	for _, current := range value {
		if current == 0 {
			return fmt.Errorf("%s cannot contain NUL", field)
		}
		if !unicode.IsControl(current) {
			continue
		}
		if policy == controlTextLines &&
			(current == '\n' || current == '\r' || current == '\t') {
			continue
		}
		return fmt.Errorf("%s cannot contain control characters", field)
	}
	return nil
}

func validateCanonicalRef(ref, field string, headsOnly bool) error {
	if err := validateText(ref, field, maxRefBytes, controlNone); err != nil {
		return err
	}
	if strings.TrimSpace(ref) != ref || !strings.HasPrefix(ref, "refs/") ||
		strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "@{") ||
		strings.ContainsAny(ref, " ~^:?*[\\") {
		return fmt.Errorf("%s must be a canonical full ref", field)
	}
	if headsOnly && !strings.HasPrefix(ref, "refs/heads/") {
		return fmt.Errorf("%s must be a canonical refs/heads ref", field)
	}
	components := strings.Split(ref, "/")
	if len(components) < 3 {
		return fmt.Errorf("%s must be a canonical full ref", field)
	}
	for _, component := range components {
		if component == "" || strings.HasPrefix(component, ".") ||
			strings.HasSuffix(component, ".lock") {
			return fmt.Errorf("%s must be a canonical full ref", field)
		}
	}
	return nil
}

func validateOID(value, field string) error {
	if err := validateText(value, field, sha256.Size*2, controlNone); err != nil {
		return err
	}
	if len(value) != 40 && len(value) != sha256.Size*2 {
		return fmt.Errorf("%s must be a full SHA-1 or SHA-256 object ID", field)
	}
	if value != strings.ToLower(value) {
		return fmt.Errorf("%s must use lowercase hexadecimal", field)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return fmt.Errorf("%s must use lowercase hexadecimal", field)
	}
	allZero := true
	for _, current := range decoded {
		if current != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return fmt.Errorf("%s cannot be the zero object ID", field)
	}
	return nil
}

func validateSameOIDFormat(left, right, field string) error {
	if len(left) != len(right) {
		return fmt.Errorf(
			"%s must use one repository object ID format",
			field,
		)
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validateTitle(value string) error {
	if err := validateText(value, "pull request title", maxTitleBytes, controlNone); err != nil {
		return err
	}
	if strings.TrimSpace(value) != value {
		return errors.New("pull request title cannot have surrounding whitespace")
	}
	return nil
}
