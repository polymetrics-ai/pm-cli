package compile

import (
	"fmt"
	"strings"
	"testing"
)

func TestNoSecretOrPolicyOverlayEscape(t *testing.T) {
	base := `{
  "schema_version":"pm.semantics/v1",
  "connector_id":"synthetic",
  "source_digest":"sha256:%s",
  "revision":"sha256:%s",
  "operations":[],
  %s
}`
	for _, test := range []struct {
		name  string
		field string
	}{
		{name: "origin expansion", field: `"origins":["https://attacker.invalid"]`},
		{name: "credential injection", field: `"credentials":{"token":"secret"}`},
		{name: "executable overlay", field: `"executable":"/bin/sh"`},
		{name: "capability expansion", field: `"capability_requirements":[{"capability":"arbitrary.execute","foundation":{"id":"FND-99","version":"1.0.0"}}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := fmt.Sprintf(base, strings.Repeat("1", 64), strings.Repeat("2", 64), test.field)
			if _, err := DecodeSemantics([]byte(document)); err == nil {
				t.Fatalf("unsafe semantic overlay %s was accepted", test.name)
			}
		})
	}

	previous := "sha256:" + strings.Repeat("3", 64)
	current := "sha256:" + strings.Repeat("4", 64)
	if err := ValidateRevisionDiff(previous, current, nil); err == nil {
		t.Fatal("changed source identity without an explicit revision diff was accepted")
	}
	diff := &SourceRevisionDiff{
		SchemaVersion:  SourceRevisionDiffSchemaVersion,
		PreviousDigest: previous,
		CurrentDigest:  current,
		Changes: []PointerChange{{
			SourceID: "root", Pointer: "/paths/~1widgets/get",
			PreviousSHA256: "sha256:" + strings.Repeat("5", 64),
			CurrentSHA256:  "sha256:" + strings.Repeat("6", 64),
		}},
	}
	if err := ValidateRevisionDiff(previous, current, diff); err != nil {
		t.Fatalf("exact pointer-level revision diff was rejected: %v", err)
	}
	if err := ValidateRevisionDiff(current, current, diff); err == nil {
		t.Fatal("revision diff was accepted without a source identity change")
	}
}
