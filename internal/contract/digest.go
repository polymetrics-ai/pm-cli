package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

type SemanticDigest string
type SourceHash string
type PayloadHash string

func CanonicalDigest(document []byte) (SemanticDigest, error) {
	var value any
	if err := decodeStrictJSON(document, &value); err != nil {
		return "", err
	}
	var canonical bytes.Buffer
	if err := appendCanonicalJSON(&canonical, value); err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical.Bytes())
	return SemanticDigest("sha256:" + hex.EncodeToString(digest[:])), nil
}

func RawSourceHash(source []byte) SourceHash {
	digest := sha256.Sum256(source)
	return SourceHash("sha256:" + hex.EncodeToString(digest[:]))
}

func NewPayloadHash(payload []byte) PayloadHash {
	digest := sha256.Sum256(payload)
	return PayloadHash("sha256:" + hex.EncodeToString(digest[:]))
}

func (d SemanticDigest) Validate() error {
	if !validSHA256(string(d)) {
		return contractError(ErrInvalidIdentity, "semantic_digest", "must be a sha256 digest")
	}
	return nil
}

func (h SourceHash) Validate() error {
	if !validSHA256(string(h)) {
		return contractError(ErrInvalidIdentity, "source_hash", "must be a sha256 digest")
	}
	return nil
}

func (h PayloadHash) Validate() error {
	if !validSHA256(string(h)) {
		return contractError(ErrInvalidDispatch, "payload_hash", "must be a sha256 digest")
	}
	return nil
}

func appendCanonicalJSON(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		if typed {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case string:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		output.Write(encoded)
	case json.Number:
		canonical, err := canonicalJSONNumber(typed)
		if err != nil {
			return err
		}
		output.WriteString(canonical)
	case []any:
		output.WriteByte('[')
		for index, element := range typed {
			if index != 0 {
				output.WriteByte(',')
			}
			if err := appendCanonicalJSON(output, element); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index != 0 {
				output.WriteByte(',')
			}
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return err
			}
			output.Write(encodedKey)
			output.WriteByte(':')
			if err := appendCanonicalJSON(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return contractError(ErrInvalidContract, "$", fmt.Sprintf("unsupported canonical JSON node %T", value))
	}
	return nil
}

func canonicalJSONNumber(number json.Number) (string, error) {
	text := number.String()
	negative := strings.HasPrefix(text, "-")
	if negative {
		text = strings.TrimPrefix(text, "-")
	}
	exponentText := ""
	if exponentIndex := strings.IndexAny(text, "eE"); exponentIndex >= 0 {
		exponentText = text[exponentIndex+1:]
		text = text[:exponentIndex]
	}
	integer := text
	fraction := ""
	if decimalIndex := strings.IndexByte(text, '.'); decimalIndex >= 0 {
		integer = text[:decimalIndex]
		fraction = text[decimalIndex+1:]
	}
	digits := strings.TrimLeft(integer+fraction, "0")
	if digits == "" {
		return "0", nil
	}
	var exponent big.Int
	if exponentText != "" {
		if _, ok := exponent.SetString(exponentText, 10); !ok {
			return "", contractError(ErrInvalidContract, "$", "number exponent is malformed")
		}
	}
	exponent.Sub(&exponent, big.NewInt(int64(len(fraction))))
	trimmed := strings.TrimRight(digits, "0")
	exponent.Add(&exponent, big.NewInt(int64(len(digits)-len(trimmed))))
	digits = trimmed
	if negative {
		digits = "-" + digits
	}
	if exponent.Sign() == 0 {
		return digits, nil
	}
	return digits + "e" + exponent.String(), nil
}
