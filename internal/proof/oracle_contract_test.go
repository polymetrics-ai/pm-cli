package proof

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestLookupJSONPointerPreservesAbsenceAndNull(t *testing.T) {
	document := json.RawMessage(`{"a/b":{"~key":null},"items":[{"id":1}]}`)
	selected, present, err := LookupJSONPointer(document, "/a~1b/~0key")
	if err != nil {
		t.Fatalf("lookup escaped pointer: %v", err)
	}
	if !present || string(selected) != "null" {
		t.Fatalf("escaped null lookup = (%s, %t); want explicit null", selected, present)
	}
	selected, present, err = LookupJSONPointer(document, "/missing")
	if err != nil {
		t.Fatalf("lookup missing pointer: %v", err)
	}
	if present || selected != nil {
		t.Fatalf("missing lookup = (%s, %t); want absent", selected, present)
	}
	selected, present, err = LookupJSONPointer(document, "")
	if err != nil || !present || len(selected) == 0 {
		t.Fatalf("root lookup = (%s, %t, %v); want complete present document", selected, present, err)
	}
	selected[0] = '['
	if document[0] != '{' {
		t.Fatal("pointer lookup returned an alias into the observed document")
	}
	if _, _, err := LookupJSONPointer(document, "/bad~2escape"); err == nil {
		t.Fatal("malformed RFC 6901 pointer was accepted")
	}
	if _, _, err := LookupJSONPointer(json.RawMessage(`{"id":1,"id":2}`), "/id"); err == nil {
		t.Fatal("ambiguous observed object was accepted")
	}
}

func TestAssertExpectedDistinguishesAbsenceNullAndType(t *testing.T) {
	missing, err := AssertExpected(
		oracleAssertionForTest("absent", "/missing", json.RawMessage(`null`)),
		OracleObservation{Document: json.RawMessage(`{"value":null}`)},
	)
	if err != nil {
		t.Fatalf("assert missing: %v", err)
	}
	if missing.Passed == nil || !*missing.Passed {
		t.Fatal("missing pointer did not satisfy absent assertion")
	}
	var absent contract.PresentValue
	if err := json.Unmarshal(missing.Observed, &absent); err != nil || absent.Presence() != contract.PresenceAbsent {
		t.Fatalf("missing observation = %s (%v); want tagged absent value", missing.Observed, err)
	}

	explicitNull, err := AssertExpected(
		oracleAssertionForTest("absent", "/value", json.RawMessage(`null`)),
		OracleObservation{Document: json.RawMessage(`{"value":null}`)},
	)
	if err != nil {
		t.Fatalf("assert explicit null: %v", err)
	}
	if explicitNull.Passed == nil || *explicitNull.Passed || string(explicitNull.Observed) != "null" {
		t.Fatalf("explicit null absence result = %#v", explicitNull)
	}

	typeMismatch, err := AssertExpected(
		oracleAssertionForTest("equals", "/value", json.RawMessage(`1`)),
		OracleObservation{Document: json.RawMessage(`{"value":"1"}`)},
	)
	if err != nil {
		t.Fatalf("assert type mismatch: %v", err)
	}
	if typeMismatch.Passed == nil || *typeMismatch.Passed {
		t.Fatal("string was coerced to an expected number")
	}
}

func TestAssertExpectedExactStructuredKindsRejectWrongRecord(t *testing.T) {
	const exact = `900719925474099312345678901234567890`
	for _, kind := range []string{"equals", "row", "effect", "cursor_transition"} {
		t.Run(kind, func(t *testing.T) {
			result, err := AssertExpected(
				oracleAssertionForTest(kind, "/selected", json.RawMessage(`{"id":`+exact+`,"state":"ready"}`)),
				OracleObservation{Document: json.RawMessage(`{"selected":{"state":"ready","id":` + exact + `}}`)},
			)
			if err != nil {
				t.Fatalf("assert exact %s: %v", kind, err)
			}
			if result.Passed == nil || !*result.Passed {
				t.Fatalf("exact %s comparison failed: %#v", kind, result)
			}
		})
	}

	wrongRecord, err := AssertExpected(
		oracleAssertionForTest("row", "/record", json.RawMessage(`{"id":7,"name":"expected"}`)),
		OracleObservation{Document: json.RawMessage(`{"status":200,"record":{"id":8,"name":"wrong"},"count":1}`)},
	)
	if err != nil {
		t.Fatalf("assert wrong record: %v", err)
	}
	if wrongRecord.Passed == nil || *wrongRecord.Passed {
		t.Fatal("200 response and matching count hid the wrong record")
	}
}

func TestAssertExpectedContainsUsesRecursiveMultiplicityAwareMatching(t *testing.T) {
	result, err := AssertExpected(
		oracleAssertionForTest("contains", "/rows", json.RawMessage(`[{"a":1},{"a":1,"b":2}]`)),
		OracleObservation{Document: json.RawMessage(`{"rows":[{"a":1,"b":2,"extra":true},{"a":1}]}`)},
	)
	if err != nil {
		t.Fatalf("assert recursive containment: %v", err)
	}
	if result.Passed == nil || !*result.Passed {
		t.Fatal("valid one-to-one recursive containment did not pass")
	}

	multiplicity, err := AssertExpected(
		oracleAssertionForTest("contains", "/rows", json.RawMessage(`[{"id":1},{"id":1}]`)),
		OracleObservation{Document: json.RawMessage(`{"rows":[{"id":1},{"id":2}]}`)},
	)
	if err != nil {
		t.Fatalf("assert multiplicity: %v", err)
	}
	if multiplicity.Passed == nil || *multiplicity.Passed {
		t.Fatal("one observed member satisfied two expected occurrences")
	}

	wrongType, err := AssertExpected(
		oracleAssertionForTest("contains", "/rows", json.RawMessage(`[1]`)),
		OracleObservation{Document: json.RawMessage(`{"rows":["1"]}`)},
	)
	if err != nil {
		t.Fatalf("assert type-safe containment: %v", err)
	}
	if wrongType.Passed == nil || *wrongType.Passed {
		t.Fatal("array containment coerced a string to a number")
	}
}

func TestAssertExpectedHashesActualBytesOnly(t *testing.T) {
	payload := []byte("independently obtained payload")
	expected := json.RawMessage(`"sha256:1888e61d147a1d33d8a4f1fc918060b14a153249d36d3694b35fc539125aac43"`)
	assertion := oracleAssertionForTest("sha256", "", expected)
	passed, err := AssertExpected(assertion, OracleObservation{Binary: payload, BinaryPresent: true})
	if err != nil {
		t.Fatalf("hash present bytes: %v", err)
	}
	if passed.Passed == nil || !*passed.Passed {
		t.Fatal("actual payload bytes did not satisfy their authored digest")
	}

	declaredOnly, err := AssertExpected(assertion, OracleObservation{Document: expected, BinaryPresent: false})
	if err != nil {
		t.Fatalf("hash absent bytes: %v", err)
	}
	if declaredOnly.Passed == nil || *declaredOnly.Passed {
		t.Fatal("provider-declared digest was hashed as though bytes were downloaded")
	}

	emptyExpected := json.RawMessage(`"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`)
	empty, err := AssertExpected(
		oracleAssertionForTest("sha256", "", emptyExpected),
		OracleObservation{Binary: []byte{}, BinaryPresent: true},
	)
	if err != nil || empty.Passed == nil || !*empty.Passed {
		t.Fatalf("present empty bytes = %#v, %v; want legitimate hash pass", empty, err)
	}
}

func TestAssertExpectedRejectsMalformedOrOversizedEvidence(t *testing.T) {
	if _, err := AssertExpected(
		oracleAssertionForTest("equals", "/id", json.RawMessage(`1`)),
		OracleObservation{Document: json.RawMessage(`{"id":1,"id":2}`)},
	); err == nil {
		t.Fatal("ambiguous observation was treated as an ordinary mismatch")
	}

	oversized, err := json.Marshal(strings.Repeat("x", 16385))
	if err != nil {
		t.Fatalf("encode oversized expected value: %v", err)
	}
	if _, err := AssertExpected(
		oracleAssertionForTest("equals", "/value", oversized),
		OracleObservation{Document: json.RawMessage(`{"value":"small"}`)},
	); err == nil {
		t.Fatal("oracle emitted evidence outside proof receipt schema bounds")
	}
}

func oracleAssertionForTest(kind, pointer string, expected json.RawMessage) contract.ScenarioAssertion {
	return contract.ScenarioAssertion{
		AssertionID: "assertion", StepID: "step", Kind: kind, Pointer: pointer, Expected: expected,
	}
}
