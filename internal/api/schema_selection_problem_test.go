package api

import "testing"

// Check: the client decodes the read refusal of ADR 0007 as a confirmed
// refusal and maps it to exit code 4 (S7). The stderr line of pm starts with
// the problem code. Defect it catches: a code or retry action that is missing
// from the controls table, so the client rejects the answer as malformed and
// pm exits with another code.
func TestSchemaSelectionAbsentIsAConfirmedRefusalWithExitFour(t *testing.T) {
	decoded, err := DecodeProblem(409, "application/problem+json",
		problemFixture(409, "schema_selection_absent", "after_capability_change"))
	if err != nil {
		t.Fatalf("DecodeProblem(409 schema_selection_absent): %v", err)
	}
	if decoded.Disposition != ProblemConfirmedRefusal {
		t.Fatalf("disposition = %q; want %q", decoded.Disposition, ProblemConfirmedRefusal)
	}
	if code := ExitCodeForProblem(decoded); code != 4 {
		t.Fatalf("exit code = %d; want 4", code)
	}
	problem := &ProblemError{Decoded: decoded}
	if message := problem.Error(); len(message) < len("schema_selection_absent:") ||
		message[:len("schema_selection_absent:")] != "schema_selection_absent:" {
		t.Fatalf("problem message = %q; want it to start with the problem code", message)
	}
	// Another retry action for the same code is not the declared row.
	if _, err := DecodeProblem(409, "application/problem+json",
		problemFixture(409, "schema_selection_absent", "correct_input")); err == nil {
		t.Fatal("DecodeProblem accepted schema_selection_absent with an undeclared retry action")
	}
}
