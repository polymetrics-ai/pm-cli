package contract

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPresencePrecisionAndMedia(t *testing.T) {
	t.Run("pm-typed-values-source-contracts-E02-C01_integer_beyond_binary64", func(t *testing.T) {
		original := mustIntegerValueForTest(t, "9007199254740993")
		decoded, encoded := roundTripPresentValueForTest(t, original)
		number, ok := decoded.ExactNumber()
		if !ok || number.Text != "9007199254740993" {
			t.Fatalf("round-tripped integer = %#v, %t; want exact 9007199254740993", number, ok)
		}
		if !bytes.Contains(encoded, []byte(`"9007199254740993"`)) {
			t.Fatalf("encoded integer does not retain an exact string: %s", encoded)
		}
		if bytes.Contains(encoded, []byte("9007199254740992")) {
			t.Fatalf("encoded integer was rounded through binary64: %s", encoded)
		}
	})

	t.Run("pm-typed-values-source-contracts-E02-C02_decimal_scale_four", func(t *testing.T) {
		original := mustDecimalValueForTest(t, "12345.6700", 9, 4)
		decoded, encoded := roundTripPresentValueForTest(t, original)
		number, ok := decoded.ExactNumber()
		if !ok || number.Text != "12345.6700" || number.Precision != 9 || number.Scale != 4 {
			t.Fatalf("round-tripped decimal = %#v, %t; want 12345.6700 precision 9 scale 4", number, ok)
		}
		if !bytes.Contains(encoded, []byte(`"12345.6700"`)) {
			t.Fatalf("encoded decimal loses trailing scale: %s", encoded)
		}
		if _, err := NewDecimalValue("-0.0", 2, 1); err == nil {
			t.Fatal("negative zero was accepted as a canonical decimal")
		}
		negativeFraction := mustDecimalValueForTest(t, "-0.1", 2, 1)
		if exact, ok := negativeFraction.ExactNumber(); !ok || exact.Text != "-0.1" {
			t.Fatalf("negative fractional decimal = %#v, %t; want exact -0.1", exact, ok)
		}
	})

	t.Run("pm-typed-values-source-contracts-E02-C03_presence_and_empty_values", func(t *testing.T) {
		values := []struct {
			name     string
			value    PresentValue
			presence Presence
			kind     ValueKind
		}{
			{name: "absent", value: AbsentValue(), presence: PresenceAbsent},
			{name: "null", value: NullValue(), presence: PresenceNull},
			{name: "empty_string", value: NewStringValue(""), presence: PresenceValue, kind: ValueKindString},
			{name: "empty_array", value: NewArrayValue(nil), presence: PresenceValue, kind: ValueKindArray},
			{name: "empty_object", value: NewObjectValue(map[string]PresentValue{}), presence: PresenceValue, kind: ValueKindObject},
			{name: "false", value: NewBooleanValue(false), presence: PresenceValue, kind: ValueKindBool},
			{name: "zero", value: mustIntegerValueForTest(t, "0"), presence: PresenceValue, kind: ValueKindInteger},
		}
		decoded := make([]PresentValue, len(values))
		for index, test := range values {
			t.Run(test.name, func(t *testing.T) {
				got, _ := roundTripPresentValueForTest(t, test.value)
				decoded[index] = got
				if got.Presence() != test.presence {
					t.Fatalf("presence = %q; want %q", got.Presence(), test.presence)
				}
				if test.presence == PresenceValue && got.Kind() != test.kind {
					t.Fatalf("kind = %q; want %q", got.Kind(), test.kind)
				}
			})
		}
		for left := range decoded {
			for offset := range len(decoded) - left - 1 {
				right := left + 1 + offset
				if decoded[left].Equal(decoded[right]) {
					t.Fatalf("%s and %s collapsed to the same value", values[left].name, values[right].name)
				}
			}
		}
		if text, ok := decoded[2].TextValue(); !ok || text != "" {
			t.Fatalf("empty string = %q, %t", text, ok)
		}
		if elements, ok := decoded[3].Elements(); !ok || len(elements) != 0 {
			t.Fatalf("empty array = %#v, %t", elements, ok)
		}
		if fields, ok := decoded[4].Fields(); !ok || len(fields) != 0 {
			t.Fatalf("empty object = %#v, %t", fields, ok)
		}
		providerNamedFields := map[string]PresentValue{
			"":      NewStringValue("empty-name"),
			" key ": NewStringValue("space-bearing-name"),
			"key":   NewStringValue("trimmed-name"),
		}
		decodedProviderObject, _ := roundTripPresentValueForTest(t, NewObjectValue(providerNamedFields))
		roundTrippedFields, ok := decodedProviderObject.Fields()
		if !ok || len(roundTrippedFields) != len(providerNamedFields) {
			t.Fatalf("provider object fields = %#v, %t; want every exact JSON property name", roundTrippedFields, ok)
		}
		for name, want := range providerNamedFields {
			got, present := roundTrippedFields[name]
			if !present || !got.Equal(want) {
				t.Fatalf("provider property %q = %#v, %t; want exact retained value", name, got, present)
			}
		}
		if boolean, ok := decoded[5].Boolean(); !ok || boolean {
			t.Fatalf("false = %t, %t", boolean, ok)
		}
		if number, ok := decoded[6].ExactNumber(); !ok || number.Text != "0" {
			t.Fatalf("zero = %#v, %t", number, ok)
		}
		if _, err := DecodePresentValue([]byte(`{"presence":"absent","text":null}`), "application/json"); err == nil {
			t.Fatal("null payload field was treated as absent")
		}
	})

	t.Run("pm-typed-values-source-contracts-E02-C04_source_default_is_not_supplied_input", func(t *testing.T) {
		operation := decodeOperationForTest(t, validOperationDocumentForTest())
		binding := operation.Inputs[1]
		if binding.SourceDefault == nil {
			t.Fatal("optional per_page binding lost its declared source default")
		}
		defaultNumber, ok := binding.SourceDefault.ExactNumber()
		if !ok || defaultNumber.Text != "4" {
			t.Fatalf("source default = %#v, %t; want exact integer 4", defaultNumber, ok)
		}
		supplied := AbsentValue()
		if supplied.Presence() != PresenceAbsent {
			t.Fatalf("supplied presence = %q; want absent", supplied.Presence())
		}
		if supplied.Equal(*binding.SourceDefault) {
			t.Fatal("absent supplied input collapsed into the declared source default")
		}
	})

	t.Run("pm-typed-values-source-contracts-E02-C05_instant_normalizes_offset", func(t *testing.T) {
		offset := mustInstantValueForTest(t, "2026-09-11T15:00:00+05:30")
		utc := mustInstantValueForTest(t, "2026-09-11T09:30:00Z")
		decoded, _ := roundTripPresentValueForTest(t, offset)
		text, ok := decoded.TemporalText()
		if !ok || text != "2026-09-11T09:30:00Z" {
			t.Fatalf("normalized instant = %q, %t; want 2026-09-11T09:30:00Z", text, ok)
		}
		if !decoded.Equal(utc) {
			t.Fatal("equivalent offset and UTC instants have different typed meaning")
		}
	})

	t.Run("V02_instant_preserves_subnanosecond_value", func(t *testing.T) {
		instant := mustInstantValueForTest(t, "2026-09-16T01:00:00.123456789100+05:30")
		decoded, _ := roundTripPresentValueForTest(t, instant)
		text, ok := decoded.TemporalText()
		if !ok || text != "2026-09-15T19:30:00.1234567891Z" {
			t.Fatalf("normalized instant = %q, %t; fractional value or date rollover changed", text, ok)
		}
		if !decoded.Equal(mustInstantValueForTest(t, text)) {
			t.Fatal("equivalent offset and UTC instants have different typed meaning")
		}
		if decoded.Equal(mustInstantValueForTest(t, "2026-09-15T19:30:00.1234567892Z")) {
			t.Fatal("distinct subnanosecond instants collapsed")
		}
	})

	t.Run("V02_instant_refuses_invalid_offsets_and_unrepresentable_UTC_years", func(t *testing.T) {
		for _, input := range []string{
			"2026-09-16T08:00:00+24:00",
			"2026-09-16T08:00:00+00:60",
			"2026-09-16T8:00:00Z",
			"2026-09-16T08:00:00,123Z",
			"0000-01-01T00:00:00+00:01",
			"9999-12-31T23:59:59-00:01",
		} {
			t.Run(input, func(t *testing.T) {
				if _, err := NewInstantValue(input); err == nil {
					t.Fatal("invalid or unrepresentable timestamp was accepted")
				}
			})
		}
	})

	t.Run("pm-typed-values-source-contracts-E02-C06_local_timestamp_preserves_wall_time", func(t *testing.T) {
		local := mustLocalTimestampValueForTest(t, "2026-09-11T15:00:00")
		decoded, _ := roundTripPresentValueForTest(t, local)
		text, ok := decoded.TemporalText()
		if !ok || text != "2026-09-11T15:00:00" {
			t.Fatalf("local timestamp = %q, %t; want unchanged wall time", text, ok)
		}
		if decoded.Kind() != ValueKindTimestampLocal {
			t.Fatalf("local timestamp kind = %q; want %q", decoded.Kind(), ValueKindTimestampLocal)
		}
	})

	t.Run("V02_declared_date_union_and_binary_reference_types", func(t *testing.T) {
		date, err := NewDateValue("2026-09-11")
		if err != nil {
			t.Fatalf("NewDateValue: %v", err)
		}
		decodedDate, _ := roundTripPresentValueForTest(t, date)
		if text, ok := decodedDate.TemporalText(); !ok || text != "2026-09-11" || decodedDate.Kind() != ValueKindDate {
			t.Fatalf("date = %q, %t, %q; want exact calendar date", text, ok, decodedDate.Kind())
		}
		if _, err := NewDateValue("2026-09-11T00:00:00Z"); err == nil {
			t.Fatal("date constructor accepted an instant")
		}

		union, err := NewUnionValue("record", NewObjectValue(map[string]PresentValue{
			"nested": NewArrayValue([]PresentValue{NewBooleanValue(false)}),
		}))
		if err != nil {
			t.Fatalf("NewUnionValue: %v", err)
		}
		decodedUnion, _ := roundTripPresentValueForTest(t, union)
		variant, member, ok := decodedUnion.UnionMember()
		if !ok || variant != "record" || member.Kind() != ValueKindObject {
			t.Fatalf("union = %q, %#v, %t; want record object variant", variant, member, ok)
		}

		wantReference := ObjectRef{
			OrgID:             "org-alpha",
			WorkspaceID:       "workspace-alpha",
			ArtifactID:        "artifact-1",
			ObjectAttemptKey:  "object-attempt-1",
			SHA256:            "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
			ByteLength:        5,
			MediaType:         "application/octet-stream",
			BackendGeneration: "generation-1",
		}
		binary, err := NewBinaryRefValue(wantReference)
		if err != nil {
			t.Fatalf("NewBinaryRefValue: %v", err)
		}
		decodedBinary, _ := roundTripPresentValueForTest(t, binary)
		reference, ok := decodedBinary.ArtifactReference()
		if !ok || reference != wantReference {
			t.Fatalf("binary reference = %#v, %t; want exact typed metadata", reference, ok)
		}
		missingScope := wantReference
		missingScope.WorkspaceID = ""
		if _, err := NewBinaryRefValue(missingScope); err == nil {
			t.Fatal("binary reference without workspace scope was accepted")
		}
		noncanonicalDigest := wantReference
		noncanonicalDigest.SHA256 = "2CF24DBA5FB0A30E26E83B2AC5B9E29E1B161E5C1FA7425E73043362938B9824"
		if _, err := NewBinaryRefValue(noncanonicalDigest); err == nil {
			t.Fatal("binary reference accepted a noncanonical SHA-256")
		}
		zeroByteReference := wantReference
		zeroByteReference.ByteLength = 0
		zeroByteBinary, err := NewBinaryRefValue(zeroByteReference)
		if err != nil {
			t.Fatalf("NewBinaryRefValue(zero bytes): %v", err)
		}
		decodedZeroByteBinary, _ := roundTripPresentValueForTest(t, zeroByteBinary)
		decodedZeroByteReference, ok := decodedZeroByteBinary.ArtifactReference()
		if !ok || decodedZeroByteReference.ByteLength != 0 {
			t.Fatalf("zero-byte artifact reference = %#v, %t; want explicit byte length zero", decodedZeroByteReference, ok)
		}
		missingByteLength := []byte(`{
		  "presence":"value",
		  "type":"binary_ref",
		  "binary":{
		    "org_id":"org-alpha",
		    "workspace_id":"workspace-alpha",
		    "artifact_id":"artifact-1",
		    "object_attempt_key":"object-attempt-1",
		    "sha256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		    "media_type":"application/octet-stream",
		    "backend_generation":"generation-1"
		  }
		}`)
		if _, err := DecodePresentValue(missingByteLength, "application/json"); err == nil {
			t.Fatal("binary reference treated an absent byte length as an explicit zero-byte artifact")
		}
	})

	t.Run("pm-typed-values-source-contracts-E02-C07_incompatible_media_is_refused", func(t *testing.T) {
		value := NewObjectValue(map[string]PresentValue{"value": NewStringValue("not an image")})
		if _, err := EncodePresentValue(value, "image/png"); err == nil {
			t.Fatal("EncodePresentValue stringified a typed object into incompatible image media")
		}
		if _, err := DecodePresentValue([]byte(`{"presence":"value","type":"string","text":"x"}`), "image/png"); err == nil {
			t.Fatal("DecodePresentValue accepted a typed JSON envelope as incompatible image media")
		}
		if _, err := ApplyResultControls(ActionResult{MediaType: "image/png", Data: NullValue()}, ResultControls{}); err == nil {
			t.Fatal("null typed action result was accepted as incompatible image media")
		}
	})

	t.Run("V02_value_ingress_rejects_lossy_unicode_replacement", func(t *testing.T) {
		invalidUTF8 := append([]byte(`{"presence":"value","type":"string","text":"`), byte(0xff))
		invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
		for index, encoded := range [][]byte{
			invalidUTF8,
			[]byte(`{"presence":"value","type":"string","text":"\ud800"}`),
			[]byte(`{"presence":"value","type":"string","text":"\udc00"}`),
		} {
			if _, err := DecodePresentValue(encoded, "application/json"); err == nil {
				t.Fatalf("lossy Unicode value case %d was accepted", index)
			}
		}
		if _, err := EncodePresentValue(NewStringValue(string([]byte{0xff})), "application/json"); err == nil {
			t.Fatal("EncodePresentValue accepted an invalid UTF-8 programmatic string")
		}
		replacement, err := DecodePresentValue([]byte(`{"presence":"value","type":"string","text":"\ufffd"}`), "application/json")
		if err != nil {
			t.Fatalf("DecodePresentValue(legitimate U+FFFD): %v", err)
		}
		if text, ok := replacement.TextValue(); !ok || text != "�" {
			t.Fatalf("legitimate U+FFFD = %q, %t", text, ok)
		}
		pair, err := DecodePresentValue([]byte(`{"presence":"value","type":"string","text":"\ud83d\ude00"}`), "application/json")
		if err != nil {
			t.Fatalf("DecodePresentValue(valid surrogate pair): %v", err)
		}
		if text, ok := pair.TextValue(); !ok || text != "😀" {
			t.Fatalf("valid surrogate pair = %q, %t", text, ok)
		}
	})

	t.Run("F02_complete_default_preserves_binary_action_results", func(t *testing.T) {
		binary, err := NewBinaryRefValue(ObjectRef{
			OrgID:             "org-alpha",
			WorkspaceID:       "workspace-alpha",
			ArtifactID:        "artifact-binary-result",
			ObjectAttemptKey:  "object-attempt-binary-result",
			SHA256:            "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
			ByteLength:        5,
			MediaType:         "application/octet-stream",
			BackendGeneration: "generation-1",
		})
		if err != nil {
			t.Fatalf("NewBinaryRefValue: %v", err)
		}
		input := ActionResult{MediaType: "application/octet-stream", Data: binary}
		complete, err := ApplyResultControls(input, ResultControls{})
		if err != nil {
			t.Fatalf("ApplyResultControls(binary default): %v", err)
		}
		if complete.MediaType != input.MediaType || !complete.Data.Equal(input.Data) {
			t.Fatalf("binary default changed the action result:\n got %#v\nwant %#v", complete, input)
		}
		if _, err := ApplyResultControls(ActionResult{MediaType: "image/png", Data: binary}, ResultControls{}); err == nil {
			t.Fatal("binary action result accepted media different from its registered reference")
		}
	})

	t.Run("F02_complete_default_and_optional_typed_controls", func(t *testing.T) {
		input := actionResultFixtureForTest(t)
		complete, err := ApplyResultControls(input, ResultControls{})
		if err != nil {
			t.Fatalf("ApplyResultControls(default): %v", err)
		}
		if complete.MediaType != input.MediaType || !complete.Data.Equal(input.Data) {
			t.Fatalf("default action result lost complete data:\n got %#v\nwant %#v", complete, input)
		}
		completeRecords := actionRecordsForTest(t, complete)
		if _, present := objectFieldsForTest(t, completeRecords[0])["pull_request"]; present {
			t.Fatal("complete result fabricated pull_request for a provider entry where it was absent")
		}
		pullRequest, present := objectFieldsForTest(t, completeRecords[1])["pull_request"]
		if !present {
			t.Fatal("complete result dropped a provider entry containing pull_request")
		}
		pullRequestURL, present := objectFieldsForTest(t, pullRequest)["url"]
		if text, ok := pullRequestURL.TextValue(); !present || !ok || text != "https://api.github.test/pulls/2" {
			t.Fatalf("preserved pull_request URL = %q, %t, %t; want exact source value", text, present, ok)
		}
		if complete.Pagination == nil || complete.Pagination.EffectiveWireLimit == nil ||
			*complete.Pagination.EffectiveWireLimit != 100 || complete.Pagination.FetchedCount != 4 ||
			complete.Pagination.ReturnedCount != 4 || complete.Pagination.EmittedCount != 4 {
			t.Fatalf("default action result lost truthful pagination: %#v", complete.Pagination)
		}
		if continuation, ok := complete.Pagination.Continuation.TextValue(); !ok || continuation != "next-page" {
			t.Fatalf("default continuation = %q, %t; want next-page", continuation, ok)
		}
		*complete.Pagination.EffectiveWireLimit = 50
		if got := *input.Pagination.EffectiveWireLimit; got != 100 {
			t.Fatalf("default result pagination aliased the input wire limit: got %d, want 100", got)
		}

		selection := &FieldSelection{Paths: []FieldPath{{"details", "labels"}}}
		selected, err := ApplyResultControls(input, ResultControls{Selection: selection})
		if err != nil {
			t.Fatalf("ApplyResultControls(selection): %v", err)
		}
		records := actionRecordsForTest(t, selected)
		firstFields := objectFieldsForTest(t, records[0])
		if len(firstFields) != 1 {
			t.Fatalf("selected first record fields = %#v; want only nested details", firstFields)
		}
		details, ok := firstFields["details"]
		if !ok {
			t.Fatal("selected nested details value is absent")
		}
		detailFields := objectFieldsForTest(t, details)
		if len(detailFields) != 1 {
			t.Fatalf("selected detail fields = %#v; want only labels", detailFields)
		}
		labels, ok := detailFields["labels"]
		if !ok {
			t.Fatal("selected labels value is absent")
		}
		originalDetails := objectFieldsForTest(t, actionRecordsForTest(t, input)[0])["details"]
		originalLabels := objectFieldsForTest(t, originalDetails)["labels"]
		if !labels.Equal(originalLabels) {
			t.Fatal("selected nested value was flattened or otherwise rewritten")
		}
		lastFields := objectFieldsForTest(t, records[len(records)-1])
		if _, fabricated := lastFields["details"]; fabricated {
			t.Fatal("selection fabricated null for an absent provider field")
		}

		emptyNameSelected, err := ApplyResultControls(input, ResultControls{
			Selection: &FieldSelection{Paths: []FieldPath{{""}}},
		})
		if err != nil {
			t.Fatalf("ApplyResultControls(empty provider property name): %v", err)
		}
		emptyNameFields := objectFieldsForTest(t, actionRecordsForTest(t, emptyNameSelected)[0])
		emptyNameValue, present := emptyNameFields[""]
		if text, ok := emptyNameValue.TextValue(); !present || !ok || text != "empty provider property" {
			t.Fatalf("selected empty-name property = %q, %t, %t; want exact provider value", text, present, ok)
		}

		filters := []struct {
			name   string
			filter RecordFilter
			wantID string
		}{
			{
				name:   "integer_zero_does_not_match_string_zero",
				filter: RecordFilter{Path: FieldPath{"marker"}, Operator: FilterEqual, Value: new(mustIntegerValueForTest(t, "0"))},
				wantID: "1",
			},
			{
				name:   "string_zero_is_distinct",
				filter: RecordFilter{Path: FieldPath{"marker"}, Operator: FilterEqual, Value: new(NewStringValue("0"))},
				wantID: "2",
			},
			{
				name:   "null_is_distinct",
				filter: RecordFilter{Path: FieldPath{"marker"}, Operator: FilterIsNull},
				wantID: "3",
			},
			{
				name:   "absent_is_distinct",
				filter: RecordFilter{Path: FieldPath{"marker"}, Operator: FilterIsAbsent},
				wantID: "4",
			},
		}
		for _, test := range filters {
			t.Run(test.name, func(t *testing.T) {
				selection := &FieldSelection{Paths: []FieldPath{{"id"}}}
				filtered, err := ApplyResultControls(input, ResultControls{Selection: selection, Filter: &test.filter})
				if err != nil {
					t.Fatalf("ApplyResultControls(filter): %v", err)
				}
				records := actionRecordsForTest(t, filtered)
				if len(records) != 1 {
					t.Fatalf("filtered records = %d; want 1", len(records))
				}
				fields := objectFieldsForTest(t, records[0])
				if len(fields) != 1 {
					t.Fatalf("filtered selected fields = %#v; filter must evaluate before selection", fields)
				}
				if got := exactRecordIDForTest(t, records[0]); got != test.wantID {
					t.Fatalf("filtered record id = %s; want %s", got, test.wantID)
				}
				if filtered.Pagination == nil || !filtered.Pagination.Continuation.Equal(input.Pagination.Continuation) {
					t.Fatal("filtering rewrote source continuation")
				}
				if filtered.Pagination.EmittedCount != 1 || filtered.Pagination.ReturnedCount != 4 || filtered.Pagination.FetchedCount != 4 {
					t.Fatalf("filtered counts = %#v; want one emitted record and four source records", filtered.Pagination)
				}
			})
		}

		invalid := []ResultControls{
			{Selection: &FieldSelection{Paths: []FieldPath{{}}}},
			{Selection: &FieldSelection{Paths: []FieldPath{{"id"}, {"id"}}}},
			{Filter: &RecordFilter{Path: FieldPath{}, Operator: FilterIsAbsent}},
			{Filter: &RecordFilter{Path: FieldPath{"marker"}, Operator: FilterEqual}},
			{Filter: &RecordFilter{Path: FieldPath{"marker"}, Operator: FilterIsNull, Value: new(NullValue())}},
			{Filter: &RecordFilter{Path: FieldPath{"marker"}, Operator: FilterOperator("contains"), Value: new(NewStringValue("0"))}},
		}
		for index, controls := range invalid {
			if _, err := ApplyResultControls(input, controls); err == nil {
				t.Fatalf("invalid result controls case %d was accepted", index)
			}
		}
	})

	t.Run("F02_empty_filtered_page_preserves_source_pagination", func(t *testing.T) {
		input := actionResultFixtureForTest(t)
		tail := NewObjectValue(map[string]PresentValue{"id": mustIntegerValueForTest(t, "5")})
		input.Pagination.UnreadTail = []PresentValue{tail}
		input.Pagination.TotalRecordCap = new(uint64(4))
		input.Pagination.FetchedCount = 5
		input.Pagination.ReturnedCount = 5
		filtered, err := ApplyResultControls(input, ResultControls{
			Filter: &RecordFilter{Path: FieldPath{"marker"}, Operator: FilterEqual, Value: new(NewStringValue("no-match"))},
		})
		if err != nil {
			t.Fatalf("ApplyResultControls: %v", err)
		}
		if records := actionRecordsForTest(t, filtered); len(records) != 0 {
			t.Fatalf("filtered records = %d; want zero", len(records))
		}
		page := filtered.Pagination
		if page == nil || page.EmittedCount != 0 || page.FetchedCount != 5 || page.ReturnedCount != 5 {
			t.Fatalf("filtered counts = %#v; source counts and emitted count must stay distinct", page)
		}
		if page.SourceExhausted || !page.Continuation.Equal(input.Pagination.Continuation) ||
			page.EffectiveWireLimit == nil || *page.EffectiveWireLimit != 100 ||
			*page.TotalRecordCap != 4 || *page.ProviderTotal != *input.Pagination.ProviderTotal {
			t.Fatalf("empty filtered page rewrote source navigation or limits: %#v", page)
		}
		if len(page.UnreadTail) != 1 || !page.UnreadTail[0].Equal(tail) {
			t.Fatal("filtering changed the unread source tail")
		}
		if input.Pagination.EmittedCount != 4 {
			t.Fatal("filtering mutated the input result")
		}
	})
	t.Run("source_no_content_has_no_invented_media", func(t *testing.T) {
		result := ActionResult{Data: AbsentValue()}
		if err := result.Validate(); err != nil {
			t.Fatalf("source-declared no-content result: %v", err)
		}
		document, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(document, &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields["media_type"]; present {
			t.Fatal("no-content result invented a media type")
		}
		var decoded ActionResult
		if err := json.Unmarshal(document, &decoded); err != nil {
			t.Fatal(err)
		}
		if err := decoded.Validate(); err != nil || decoded.Data.Presence() != PresenceAbsent {
			t.Fatalf("no-content result did not retain explicit absence: %v", err)
		}
		if (ActionResult{MediaType: "application/json", Data: AbsentValue()}).Validate() == nil {
			t.Fatal("no-content result accepted an invented media type")
		}
		if (ActionResult{Data: AbsentValue(), Pagination: actionResultFixtureForTest(t).Pagination}).Validate() == nil {
			t.Fatal("no-content result accepted pagination")
		}
		if (ActionResult{Data: PresentValue{}}).Validate() == nil {
			t.Fatal("zero PresentValue was treated as source-declared no-content")
		}
		for _, data := range []PresentValue{NullValue(), NewArrayValue(nil)} {
			if (ActionResult{Data: data}).Validate() == nil {
				t.Fatal("actual null or empty-array content was accepted without declared media")
			}
		}
	})
	t.Run("unsent_wire_limit_remains_unknown_after_result_controls", func(t *testing.T) {
		input := actionResultFixtureForTest(t)
		document, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(document, &fields); err != nil {
			t.Fatal(err)
		}
		var page map[string]json.RawMessage
		if err := json.Unmarshal(fields["pagination"], &page); err != nil {
			t.Fatal(err)
		}
		page["effective_wire_limit"] = json.RawMessage("null")
		fields["pagination"], err = json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		document, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ActionResult
		if err := json.Unmarshal(document, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Pagination == nil || decoded.Pagination.EffectiveWireLimit != nil {
			t.Fatalf("decoded unsent wire limit = %#v; want nil", decoded.Pagination)
		}
		zero := uint64(0)
		decoded.Pagination.EffectiveWireLimit = &zero
		if err := decoded.Validate(); err == nil {
			t.Fatal("present zero wire limit was accepted")
		}
		decoded.Pagination.EffectiveWireLimit = nil
		controlled, err := ApplyResultControls(decoded, ResultControls{
			Selection: &FieldSelection{Paths: []FieldPath{{"id"}}},
		})
		if err != nil {
			t.Fatalf("actual unsent page-size field was refused: %v", err)
		}
		document, err = json.Marshal(controlled.Pagination)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(document, &page); err != nil {
			t.Fatal(err)
		}
		if string(page["effective_wire_limit"]) != "null" || controlled.Pagination.SourceExhausted ||
			!controlled.Pagination.Continuation.Equal(input.Pagination.Continuation) {
			t.Fatal("result controls invented a sent limit or source exhaustion")
		}
	})
}

func TestResultControlsCapAndContinuationPresence(t *testing.T) {
	t.Run("nil controls retain complete-default encoding", func(t *testing.T) {
		controls := ResultControls{}
		if controls.HasContinuation() || !controls.ContinuationValue().Equal(AbsentValue()) {
			t.Fatalf("omitted continuation state = %#v", controls.ContinuationValue())
		}
		encoded, err := json.Marshal(controls)
		if err != nil {
			t.Fatalf("marshal default controls: %v", err)
		}
		if string(encoded) != `{}` {
			t.Fatalf("default controls encoding = %s; want {}", encoded)
		}
	})

	t.Run("explicit absence remains distinct from omission", func(t *testing.T) {
		controls := ResultControls{Continuation: new(AbsentValue())}
		if err := controls.Validate(); err != nil {
			t.Fatalf("validate explicit absent continuation: %v", err)
		}
		if controls.HasContinuation() || !controls.ContinuationValue().Equal(AbsentValue()) {
			t.Fatalf("explicit absent continuation state = %#v", controls.ContinuationValue())
		}
		encoded, err := json.Marshal(controls)
		if err != nil {
			t.Fatalf("marshal explicit absent continuation: %v", err)
		}
		if string(encoded) != `{"continuation":{"presence":"absent"}}` {
			t.Fatalf("explicit absent continuation encoding = %s", encoded)
		}
	})

	t.Run("positive cap and bounded opaque resume are exact", func(t *testing.T) {
		cap := ^uint64(0)
		token := strings.Repeat("é", maxResultContinuationBytes/2)
		continuation := NewStringValue(token)
		controls := ResultControls{TotalRecordCap: &cap, Continuation: &continuation}
		if err := controls.Validate(); err != nil {
			t.Fatalf("validate cap and boundary continuation: %v", err)
		}
		if !controls.HasContinuation() || !controls.ContinuationValue().Equal(continuation) {
			t.Fatal("continuation helper lost the exact bounded opaque value")
		}
		text, ok := controls.ContinuationValue().TextValue()
		if !ok || text != token || len(text) != maxResultContinuationBytes {
			t.Fatalf("continuation text length = %d, %t; want exact %d-byte token", len(text), ok, maxResultContinuationBytes)
		}
	})

	for _, test := range []struct {
		name     string
		controls ResultControls
	}{
		{name: "zero total cap", controls: ResultControls{TotalRecordCap: new(uint64(0))}},
		{name: "typed null continuation", controls: ResultControls{Continuation: new(NullValue())}},
		{name: "empty continuation", controls: ResultControls{Continuation: new(NewStringValue(""))}},
		{name: "non-string continuation", controls: ResultControls{Continuation: new(NewBooleanValue(false))}},
		{name: "invalid typed state", controls: ResultControls{Continuation: new(PresentValue{})}},
		{
			name: "continuation exceeds byte bound",
			controls: ResultControls{
				Continuation: new(NewStringValue(strings.Repeat("é", maxResultContinuationBytes/2) + "x")),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.controls.Validate(); err == nil {
				t.Fatal("invalid cap or continuation was accepted")
			}
		})
	}
}

func TestPresentValueContainerIsolation(t *testing.T) {
	fields := map[string]PresentValue{
		"label": NewStringValue("original"),
		"nested": NewObjectValue(map[string]PresentValue{
			"left": NewStringValue("L"), "right": NewStringValue("R"),
		}),
	}
	elements := []PresentValue{NewObjectValue(fields)}
	value := NewArrayValue(elements)
	before, err := EncodePresentValue(value, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	fields["label"] = NewStringValue("mutated constructor input")
	elements[0] = NullValue()
	view, _ := value.Elements()
	viewFields, _ := view[0].Fields()
	viewFields["label"] = NewStringValue("mutated object view")
	nestedView, _ := viewFields["nested"].Fields()
	nestedView["left"] = NullValue()
	view[0] = NullValue()

	union, err := NewUnionValue("records", value)
	if err != nil {
		t.Fatal(err)
	}
	_, member, _ := union.UnionMember()
	memberView, _ := member.Elements()
	memberView[0] = NullValue()

	selected, err := ApplyResultControls(ActionResult{MediaType: "application/json", Data: value}, ResultControls{
		Selection: &FieldSelection{Paths: []FieldPath{{"nested", "left"}, {"nested", "right"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	selectedRows := actionRecordsForTest(t, selected)
	selectedFields := objectFieldsForTest(t, selectedRows[0])
	selectedNested := objectFieldsForTest(t, selectedFields["nested"])
	if !selectedNested["left"].Equal(NewStringValue("L")) || !selectedNested["right"].Equal(NewStringValue("R")) {
		t.Fatal("selection did not preserve independently owned nested values")
	}
	for _, retained := range []PresentValue{value, member} {
		after, err := EncodePresentValue(retained, "application/json")
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("constructor inputs, accessor views or selection mutated retained data: %s, %v", after, err)
		}
	}
}

func roundTripPresentValueForTest(t *testing.T, value PresentValue) (PresentValue, []byte) {
	t.Helper()
	encoded, err := EncodePresentValue(value, "application/json")
	if err != nil {
		t.Fatalf("EncodePresentValue: %v", err)
	}
	decoded, err := DecodePresentValue(encoded, "application/json")
	if err != nil {
		t.Fatalf("DecodePresentValue: %v", err)
	}
	if !decoded.Equal(value) {
		t.Fatalf("round trip changed value:\n got %#v\nwant %#v", decoded, value)
	}
	return decoded, encoded
}

func mustIntegerValueForTest(t *testing.T, exact string) PresentValue {
	t.Helper()
	value, err := NewIntegerValue(exact)
	if err != nil {
		t.Fatalf("NewIntegerValue(%q): %v", exact, err)
	}
	return value
}

func mustExactNumberForTest(t *testing.T, value PresentValue) ExactNumber {
	t.Helper()
	number, ok := value.ExactNumber()
	if !ok {
		t.Fatalf("value kind = %q; want exact number", value.Kind())
	}
	return number
}

func mustDecimalValueForTest(t *testing.T, exact string, precision, scale int) PresentValue {
	t.Helper()
	value, err := NewDecimalValue(exact, precision, scale)
	if err != nil {
		t.Fatalf("NewDecimalValue(%q, %d, %d): %v", exact, precision, scale, err)
	}
	return value
}

func mustInstantValueForTest(t *testing.T, value string) PresentValue {
	t.Helper()
	instant, err := NewInstantValue(value)
	if err != nil {
		t.Fatalf("NewInstantValue(%q): %v", value, err)
	}
	return instant
}

func mustLocalTimestampValueForTest(t *testing.T, value string) PresentValue {
	t.Helper()
	local, err := NewLocalTimestampValue(value)
	if err != nil {
		t.Fatalf("NewLocalTimestampValue(%q): %v", value, err)
	}
	return local
}

func actionResultFixtureForTest(t *testing.T) ActionResult {
	t.Helper()
	records := []PresentValue{
		NewObjectValue(map[string]PresentValue{
			"id":     mustIntegerValueForTest(t, "1"),
			"":       NewStringValue("empty provider property"),
			"marker": mustIntegerValueForTest(t, "0"),
			"title":  NewStringValue("integer marker"),
			"details": NewObjectValue(map[string]PresentValue{
				"enabled": NewBooleanValue(false),
				"labels":  NewArrayValue(nil),
				"meta": NewObjectValue(map[string]PresentValue{
					"Password": NewStringValue("provider-defined data"),
				}),
			}),
		}),
		NewObjectValue(map[string]PresentValue{
			"id":     mustIntegerValueForTest(t, "2"),
			"marker": NewStringValue("0"),
			"title":  NewStringValue("string marker"),
			"details": NewObjectValue(map[string]PresentValue{
				"enabled": NewBooleanValue(true),
			}),
			"pull_request": NewObjectValue(map[string]PresentValue{
				"url": NewStringValue("https://api.github.test/pulls/2"),
			}),
		}),
		NewObjectValue(map[string]PresentValue{
			"id":     mustIntegerValueForTest(t, "3"),
			"marker": NullValue(),
			"title":  NewStringValue("null marker"),
		}),
		NewObjectValue(map[string]PresentValue{
			"id":    mustIntegerValueForTest(t, "4"),
			"title": NewStringValue("absent marker and details"),
		}),
	}
	return ActionResult{
		MediaType: "application/json",
		Data:      NewArrayValue(records),
		Pagination: &ResultPagination{
			EffectiveWireLimit: new(uint64(100)),
			TotalRecordCap:     new(uint64(10)),
			Continuation:       NewStringValue("next-page"),
			UnreadTail:         []PresentValue{},
			FetchedCount:       4,
			ReturnedCount:      4,
			EmittedCount:       4,
			ProviderTotal:      new(mustExactNumberForTest(t, mustIntegerValueForTest(t, "12"))),
			SourceExhausted:    false,
		},
	}
}

func actionRecordsForTest(t *testing.T, result ActionResult) []PresentValue {
	t.Helper()
	records, ok := result.Data.Elements()
	if !ok {
		t.Fatalf("action result data kind = %q; want array", result.Data.Kind())
	}
	return records
}

func objectFieldsForTest(t *testing.T, value PresentValue) map[string]PresentValue {
	t.Helper()
	fields, ok := value.Fields()
	if !ok {
		t.Fatalf("value kind = %q; want object", value.Kind())
	}
	return fields
}

func exactRecordIDForTest(t *testing.T, record PresentValue) string {
	t.Helper()
	identifier, ok := objectFieldsForTest(t, record)["id"]
	if !ok {
		t.Fatal("selected record has no id")
	}
	number, ok := identifier.ExactNumber()
	if !ok {
		t.Fatalf("record id = %#v; want exact integer", identifier)
	}
	return number.Text
}
