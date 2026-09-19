package proof

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const maxContainmentComparisons = 1 << 20

type OracleObservation struct {
	Document      json.RawMessage
	Binary        []byte
	BinaryPresent bool
}

// LookupJSONPointer evaluates one RFC 6901 pointer without collapsing an absent
// member into explicit JSON null. Returned bytes are owned by the caller.
func LookupJSONPointer(document json.RawMessage, pointer string) (json.RawMessage, bool, error) {
	if len(pointer) > 4096 || !validRFC6901Pointer(pointer) {
		return nil, false, errors.New("JSON pointer is malformed or outside bounds")
	}
	if document == nil {
		return nil, false, nil
	}
	if _, err := contract.CanonicalDigest(document); err != nil {
		return nil, false, errors.New("observed document is malformed, ambiguous, or outside bounds")
	}
	current := append(json.RawMessage(nil), document...)
	if pointer == "" {
		return current, true, nil
	}
	for _, token := range decodeRFC6901Pointer(pointer) {
		trimmed := bytes.TrimSpace(current)
		if len(trimmed) == 0 {
			return nil, false, errors.New("observed document is malformed")
		}
		switch trimmed[0] {
		case '{':
			var object map[string]json.RawMessage
			if err := json.Unmarshal(trimmed, &object); err != nil {
				return nil, false, errors.New("observed object is malformed")
			}
			next, present := object[token]
			if !present {
				return nil, false, nil
			}
			current = append(current[:0], next...)
		case '[':
			if token == "" || token == "-" || (len(token) > 1 && token[0] == '0') {
				return nil, false, nil
			}
			index, err := strconv.ParseUint(token, 10, 63)
			if err != nil {
				return nil, false, nil
			}
			var array []json.RawMessage
			if err := json.Unmarshal(trimmed, &array); err != nil {
				return nil, false, errors.New("observed array is malformed")
			}
			if index >= uint64(len(array)) {
				return nil, false, nil
			}
			current = append(current[:0], array[index]...)
		default:
			return nil, false, nil
		}
	}
	return append(json.RawMessage(nil), current...), true, nil
}

func AssertExpected(assertion contract.ScenarioAssertion, observation OracleObservation) (contract.ProofAssertionObservation, error) {
	if err := assertion.Validate(); err != nil {
		return contract.ProofAssertionObservation{}, errors.New("oracle assertion is malformed")
	}
	result := contract.ProofAssertionObservation{
		AssertionID: assertion.AssertionID,
		Expected:    append(json.RawMessage(nil), assertion.Expected...),
	}

	if assertion.Kind == "sha256" {
		var expected string
		if err := json.Unmarshal(assertion.Expected, &expected); err != nil || contract.SourceHash(expected).Validate() != nil {
			return contract.ProofAssertionObservation{}, errors.New("sha256 expectation is malformed")
		}
		if !observation.BinaryPresent {
			absent, err := absentObservation()
			if err != nil {
				return contract.ProofAssertionObservation{}, err
			}
			result.Observed = absent
			return finishOracleObservation(result, false)
		}
		actual := contract.RawSourceHash(observation.Binary)
		encoded, err := json.Marshal(actual)
		if err != nil {
			return contract.ProofAssertionObservation{}, errors.New("actual byte hash cannot be encoded")
		}
		result.Observed = encoded
		return finishOracleObservation(result, actual == contract.SourceHash(expected))
	}

	observed, present, err := LookupJSONPointer(observation.Document, assertion.Pointer)
	if err != nil {
		return contract.ProofAssertionObservation{}, err
	}
	if !present {
		observed, err = absentObservation()
		if err != nil {
			return contract.ProofAssertionObservation{}, err
		}
	}
	result.Observed = observed

	var passed bool
	switch assertion.Kind {
	case "absent":
		passed = !present
	case "equals", "row", "effect", "cursor_transition":
		if present {
			passed, err = exactJSONEqual(assertion.Expected, observed)
		}
	case "contains":
		if present {
			var actualValue any
			actualValue, err = decodeExactJSON(observed)
			if err == nil {
				var expectedValue any
				expectedValue, err = decodeExactJSON(assertion.Expected)
				if err == nil {
					passed, err = containsJSON(actualValue, expectedValue)
				}
			}
		}
	default:
		return contract.ProofAssertionObservation{}, errors.New("oracle assertion kind is unsupported")
	}
	if err != nil {
		return contract.ProofAssertionObservation{}, err
	}
	return finishOracleObservation(result, passed)
}

func finishOracleObservation(observation contract.ProofAssertionObservation, passed bool) (contract.ProofAssertionObservation, error) {
	observation.Passed = new(bool)
	*observation.Passed = passed
	if err := observation.Validate(); err != nil {
		return contract.ProofAssertionObservation{}, errors.New("oracle observation exceeds proof receipt bounds")
	}
	return observation, nil
}

func absentObservation() (json.RawMessage, error) {
	encoded, err := json.Marshal(contract.AbsentValue())
	if err != nil {
		return nil, errors.New("absent observation cannot be encoded")
	}
	return encoded, nil
}

func exactJSONEqual(left, right json.RawMessage) (bool, error) {
	leftDigest, err := contract.CanonicalDigest(left)
	if err != nil {
		return false, errors.New("expected JSON is malformed or ambiguous")
	}
	rightDigest, err := contract.CanonicalDigest(right)
	if err != nil {
		return false, errors.New("observed JSON is malformed or ambiguous")
	}
	return leftDigest == rightDigest, nil
}

func decodeExactJSON(document json.RawMessage) (any, error) {
	if _, err := contract.CanonicalDigest(document); err != nil {
		return nil, errors.New("oracle JSON is malformed or ambiguous")
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("oracle JSON is malformed")
	}
	return value, nil
}

func containsJSON(actual, expected any) (bool, error) {
	switch wanted := expected.(type) {
	case map[string]any:
		available, ok := actual.(map[string]any)
		if !ok {
			return false, nil
		}
		for name, expectedValue := range wanted {
			actualValue, present := available[name]
			if !present {
				return false, nil
			}
			contained, err := containsJSON(actualValue, expectedValue)
			if err != nil || !contained {
				return contained, err
			}
		}
		return true, nil
	case []any:
		available, ok := actual.([]any)
		if !ok || len(wanted) > len(available) {
			return false, nil
		}
		if len(wanted) == 0 {
			return true, nil
		}
		if len(available) > maxContainmentComparisons/len(wanted) {
			return false, errors.New("containment comparison exceeds bounded work limit")
		}
		edges := make([][]bool, len(wanted))
		for expectedIndex, expectedValue := range wanted {
			edges[expectedIndex] = make([]bool, len(available))
			for actualIndex, actualValue := range available {
				contained, err := containsJSON(actualValue, expectedValue)
				if err != nil {
					return false, err
				}
				edges[expectedIndex][actualIndex] = contained
			}
		}
		matchedExpected := make([]int, len(available))
		for index := range matchedExpected {
			matchedExpected[index] = -1
		}
		for expectedIndex := range wanted {
			seen := make([]bool, len(available))
			if !augmentContainmentMatch(expectedIndex, edges, matchedExpected, seen) {
				return false, nil
			}
		}
		return true, nil
	default:
		if jsonNodeKind(actual) != jsonNodeKind(expected) {
			return false, nil
		}
		actualJSON, err := json.Marshal(actual)
		if err != nil {
			return false, errors.New("observed JSON value cannot be encoded")
		}
		expectedJSON, err := json.Marshal(expected)
		if err != nil {
			return false, errors.New("expected JSON value cannot be encoded")
		}
		return exactJSONEqual(expectedJSON, actualJSON)
	}
}

func augmentContainmentMatch(expectedIndex int, edges [][]bool, matchedExpected []int, seen []bool) bool {
	for actualIndex, compatible := range edges[expectedIndex] {
		if !compatible || seen[actualIndex] {
			continue
		}
		seen[actualIndex] = true
		if matchedExpected[actualIndex] == -1 || augmentContainmentMatch(matchedExpected[actualIndex], edges, matchedExpected, seen) {
			matchedExpected[actualIndex] = expectedIndex
			return true
		}
	}
	return false
}

func jsonNodeKind(value any) byte {
	switch value.(type) {
	case nil:
		return '0'
	case bool:
		return 'b'
	case json.Number:
		return 'n'
	case string:
		return 's'
	case []any:
		return 'a'
	case map[string]any:
		return 'o'
	default:
		return '?'
	}
}

func validRFC6901Pointer(pointer string) bool {
	if !utf8.ValidString(pointer) {
		return false
	}
	if pointer == "" {
		return true
	}
	if pointer[0] != '/' {
		return false
	}
	for index := 0; index < len(pointer); index++ {
		if pointer[index] != '~' {
			continue
		}
		if index+1 >= len(pointer) || (pointer[index+1] != '0' && pointer[index+1] != '1') {
			return false
		}
		index++
	}
	return true
}

func decodeRFC6901Pointer(pointer string) []string {
	if pointer == "" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for index := range parts {
		parts[index] = strings.ReplaceAll(strings.ReplaceAll(parts[index], "~1", "/"), "~0", "~")
	}
	return parts
}
