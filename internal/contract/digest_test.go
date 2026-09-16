package contract

import (
	"encoding/json"
	"testing"
)

func TestStableDigestAndIdentity(t *testing.T) {
	documentA := []byte(`{
  "operation_id":"issues/list-for-repo",
  "semantics":{"enabled":false,"limit":{"presence":"value","type":"integer","exact":"9007199254740993"}},
  "foundation":{"id":"FND-03","version":"1.0.0"}
}`)
	documentB := []byte(`{"foundation":{"version":"1.0.0","id":"FND-03"},"semantics":{"limit":{"exact":"9007199254740993","type":"integer","presence":"value"},"enabled":false},"operation_id":"issues/list-for-repo"}`)
	changedDocument := []byte(`{
  "operation_id":"issues/list-for-repo",
  "semantics":{"enabled":true,"limit":{"presence":"value","type":"integer","exact":"9007199254740993"}},
  "foundation":{"id":"FND-03","version":"1.0.0"}
}`)

	digestA := canonicalDigestForTest(t, documentA)
	digestB := canonicalDigestForTest(t, documentB)

	t.Run("V03_key_order_is_not_semantics", func(t *testing.T) {
		if digestA != digestB {
			t.Fatalf("canonical digests differ by object key order: %q != %q", digestA, digestB)
		}
	})

	t.Run("V03_semantic_null_remains_canonical_digest_input", func(t *testing.T) {
		left := canonicalDigestForTest(t, []byte(`{"nullable":null,"enabled":false}`))
		right := canonicalDigestForTest(t, []byte(`{"enabled":false,"nullable":null}`))
		if left != right {
			t.Fatal("semantic null or key order changed the canonical digest")
		}
	})

	t.Run("V03_equivalent_JSON_number_spellings_are_not_semantics", func(t *testing.T) {
		baseline := canonicalDigestForTest(t, []byte(`{"count":1,"zero":0}`))
		for _, document := range [][]byte{
			[]byte(`{"count":1.0,"zero":-0}`),
			[]byte(`{"count":10e-1,"zero":0.000e999999999999999999999999}`),
		} {
			if digest := canonicalDigestForTest(t, document); digest != baseline {
				t.Fatalf("equivalent JSON number spelling changed digest: %q != %q", digest, baseline)
			}
		}
		if canonicalDigestForTest(t, []byte(`{"count":1.0001,"zero":0}`)) == baseline {
			t.Fatal("changed exact JSON number retained its digest")
		}
		if canonicalDigestForTest(t, []byte(`{"exact":"1.0","scale":1}`)) == canonicalDigestForTest(t, []byte(`{"exact":"1","scale":0}`)) {
			t.Fatal("typed exact-number metadata was normalized away")
		}
	})

	t.Run("pm-typed-values-source-contracts-E03-C01_changed_semantics_changes_digest", func(t *testing.T) {
		changed := canonicalDigestForTest(t, changedDocument)
		if changed == digestA {
			t.Fatalf("changed enabled semantics retained digest %q", digestA)
		}
	})

	t.Run("V03_raw_source_hash_is_a_separate_identity_domain", func(t *testing.T) {
		rawA := RawSourceHash(documentA)
		rawB := RawSourceHash(documentB)
		if rawA == rawB {
			t.Fatal("raw source hashes ignored byte-level ordering and whitespace differences")
		}
		if RawSourceHash(documentA) != rawA {
			t.Fatal("raw source hash is not deterministic for identical bytes")
		}
		if digestA != digestB {
			t.Fatal("raw source differences leaked into the semantic digest")
		}
	})

	t.Run("pm-typed-values-source-contracts-E03-C02_semantics_and_attempts_do_not_redefine_entity", func(t *testing.T) {
		entity, err := NewEntityKey("provider-account-github-42", "issues", "9001", "generation-1")
		if err != nil {
			t.Fatalf("NewEntityKey: %v", err)
		}
		sameAccountThroughAnotherAlias, err := NewEntityKey("provider-account-github-42", "issues", "9001", "generation-1")
		if err != nil {
			t.Fatalf("NewEntityKey(alias): %v", err)
		}
		if sameAccountThroughAnotherAlias != entity {
			t.Fatal("connection alias changed canonical provider entity identity")
		}

		emptyProviderKey, err := NewEntityKey("provider-account-postgres-7", "rows", "", "generation-1")
		if err != nil {
			t.Fatalf("NewEntityKey(empty provider text key): %v", err)
		}
		spaceBearingProviderKey, err := NewEntityKey("provider-account-postgres-7", "rows", " row key ", "generation-1")
		if err != nil {
			t.Fatalf("NewEntityKey(space-bearing provider text key): %v", err)
		}
		trimmedProviderKey, err := NewEntityKey("provider-account-postgres-7", "rows", "row key", "generation-1")
		if err != nil {
			t.Fatalf("NewEntityKey(trimmed provider text key): %v", err)
		}
		if emptyProviderKey == spaceBearingProviderKey || spaceBearingProviderKey == trimmedProviderKey {
			t.Fatal("exact provider text keys collapsed empty or boundary-space distinctions")
		}
		encodedEmptyProviderKey, err := json.Marshal(emptyProviderKey)
		if err != nil {
			t.Fatalf("Marshal(empty provider text key): %v", err)
		}
		var roundTrippedEmptyProviderKey EntityKey
		if err := json.Unmarshal(encodedEmptyProviderKey, &roundTrippedEmptyProviderKey); err != nil {
			t.Fatalf("Unmarshal(empty provider text key): %v", err)
		}
		if roundTrippedEmptyProviderKey != emptyProviderKey {
			t.Fatal("empty provider text key did not survive the public JSON boundary")
		}
		var missingProviderKey EntityKey
		if err := json.Unmarshal([]byte(`{"account_witness":"provider-account-postgres-7","resource":"rows","creation_generation":"generation-1"}`), &missingProviderKey); err == nil {
			t.Fatal("missing provider key presence was confused with an explicitly empty provider text key")
		}

		recreatedEntity, err := NewEntityKey("provider-account-github-42", "issues", "9001", "generation-2")
		if err != nil {
			t.Fatalf("NewEntityKey(recreated): %v", err)
		}
		if recreatedEntity == entity {
			t.Fatal("provider key reuse across creation generations collapsed logical entity identity")
		}

		binding, err := BindSemantics(entity, digestA)
		if err != nil {
			t.Fatalf("BindSemantics: %v", err)
		}
		changedDigest := canonicalDigestForTest(t, changedDocument)
		changedBinding, err := BindSemantics(entity, changedDigest)
		if err != nil {
			t.Fatalf("BindSemantics(changed): %v", err)
		}
		if binding == changedBinding {
			t.Fatal("changed semantics did not change the semantic binding")
		}
		if binding.EntityKey != entity || changedBinding.EntityKey != entity {
			t.Fatal("nonidentity semantic revision changed logical entity identity")
		}

		dispatchID, err := NewDispatchID("dispatch-1")
		if err != nil {
			t.Fatalf("NewDispatchID: %v", err)
		}
		firstAttemptID, err := NewAttemptID("attempt-revision-1")
		if err != nil {
			t.Fatalf("NewAttemptID(first): %v", err)
		}
		secondAttemptID, err := NewAttemptID("attempt-revision-2")
		if err != nil {
			t.Fatalf("NewAttemptID(second): %v", err)
		}
		first, err := BindAttempt(binding, dispatchID, firstAttemptID)
		if err != nil {
			t.Fatalf("BindAttempt(first): %v", err)
		}
		second, err := BindAttempt(binding, dispatchID, secondAttemptID)
		if err != nil {
			t.Fatalf("BindAttempt(second): %v", err)
		}
		if first == second {
			t.Fatal("different physical attempts produced the same attempt binding")
		}
		if first.EntityKey != entity || second.EntityKey != entity {
			t.Fatal("attempt revision changed logical entity identity")
		}
		if first.SemanticDigest != digestA || second.SemanticDigest != digestA {
			t.Fatal("attempt binding changed the frozen semantic digest")
		}

		firstEffect, err := NewEffectKey(entity, "occurrence-1", "revision-1")
		if err != nil {
			t.Fatalf("NewEffectKey(first): %v", err)
		}
		exactReplay, err := NewEffectKey(entity, "occurrence-1", "revision-1")
		if err != nil {
			t.Fatalf("NewEffectKey(replay): %v", err)
		}
		secondEffect, err := NewEffectKey(entity, "occurrence-2", "revision-1")
		if err != nil {
			t.Fatalf("NewEffectKey(second occurrence): %v", err)
		}
		if firstEffect != exactReplay || firstEffect == secondEffect {
			t.Fatal("effect occurrence identity does not distinguish replay from a legitimate second action")
		}
	})

	t.Run("V03_canonical_input_is_strict_and_lossless_unicode", func(t *testing.T) {
		invalidUTF8 := append([]byte(`{"text":"`), byte(0xff))
		invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
		invalid := [][]byte{
			[]byte(`{"a":1,"a":1}`),
			[]byte(`{"a":1,"\u0061":2}`),
			[]byte(`{"a":1} {"b":2}`),
			[]byte(`{"a":`),
			invalidUTF8,
			[]byte(`{"text":"\ud800"}`),
			[]byte(`{"text":"\udc00"}`),
			[]byte(`{"text":"\ud800x"}`),
		}
		for index, document := range invalid {
			if _, err := CanonicalDigest(document); err == nil {
				t.Fatalf("invalid canonical input case %d was accepted", index)
			}
		}

		replacementRune := canonicalDigestForTest(t, []byte(`{"text":"�"}`))
		replacementEscape := canonicalDigestForTest(t, []byte(`{"text":"\ufffd"}`))
		if replacementRune != replacementEscape {
			t.Fatal("legitimate U+FFFD spellings have different semantic digests")
		}
		validPair := canonicalDigestForTest(t, []byte(`{"text":"\ud83d\ude00"}`))
		validRune := canonicalDigestForTest(t, []byte(`{"text":"😀"}`))
		if validPair != validRune {
			t.Fatal("valid UTF-16 surrogate pair did not preserve its Unicode scalar")
		}
	})
}

func canonicalDigestForTest(t *testing.T, document []byte) SemanticDigest {
	t.Helper()
	digest, err := CanonicalDigest(document)
	if err != nil {
		t.Fatalf("CanonicalDigest: %v", err)
	}
	return digest
}
