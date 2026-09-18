package proof

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestNoStaleProofResurrection(t *testing.T) {
	store, _ := openProofStore(t)
	started := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	passing := proofReceiptFixture(t, "case-stale", started)
	oldRecord, err := store.Append(t.Context(), contract.LaneEligible, passing)
	if err != nil {
		t.Fatalf("append original passing receipt: %v", err)
	}

	failed := failedProofReceipt(t, passing, "run-after-relevant-failure", started.Add(time.Hour))
	newRecord, err := store.Append(t.Context(), contract.LaneEligible, failed)
	if err != nil {
		t.Fatalf("append newer failed receipt: %v", err)
	}
	if newRecord.Sequence <= oldRecord.Sequence {
		t.Fatalf("new failure sequence = %d; want newer than %d", newRecord.Sequence, oldRecord.Sequence)
	}

	identity := currentnessFixture(t, passing).Identity
	latest, found, err := store.Latest(t.Context(), identity)
	if err != nil || !found {
		t.Fatalf("load latest receipt: found=%v err=%v", found, err)
	}
	if latest.ID != newRecord.ID || latest.Receipt.Status != contract.ProofFailed {
		t.Fatalf("latest receipt = %#v; want the newer failure", latest)
	}
	key, err := CaseKey(identity)
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := SelectCases(ModeRetry, []CaseCandidate{{
		Current:      currentnessFixture(t, passing),
		Availability: contract.LaneEligible,
		Reason:       "eligible controlled case",
	}}, map[string]ReceiptRecord{key: latest})
	if err != nil {
		t.Fatalf("select retry cases: %v", err)
	}
	if len(decisions) != 1 || !decisions[0].Execute || decisions[0].Reused ||
		decisions[0].Receipt == nil || decisions[0].Receipt.ID != newRecord.ID {
		t.Fatalf("retry decision = %#v; want only the newest failed receipt selected", decisions)
	}

	staleReport := proofLaneReport([]ReportCase{{
		Identity:     identity,
		Availability: contract.LaneEligible,
		Reason:       "eligible controlled case",
		ReceiptID:    &oldRecord.ID,
	}})
	if err := store.PublishReport(t.Context(), staleReport); err == nil {
		t.Fatal("report resurrected an older passing receipt after a newer failure")
	}
	failureReport := proofLaneReport([]ReportCase{{
		Identity:     identity,
		Availability: contract.LaneEligible,
		Reason:       "latest controlled execution failed",
		ReceiptID:    &newRecord.ID,
	}})
	if err := store.PublishReport(t.Context(), failureReport); err != nil {
		t.Fatalf("publish report containing newest failure: %v", err)
	}
	readBack, found, err := store.ReadReport(t.Context(), identity.ConnectorID, identity.Lane)
	if err != nil || !found || len(readBack.Cases) != 1 || readBack.Cases[0].ReceiptID == nil ||
		*readBack.Cases[0].ReceiptID != newRecord.ID {
		t.Fatalf("failure report readback = %#v found=%v err=%v", readBack, found, err)
	}
}

func TestProofCurrentness(t *testing.T) {
	started := time.Date(2026, 9, 19, 11, 0, 0, 0, time.UTC)
	receipt := proofReceiptFixture(t, "case-current", started)
	current := currentnessFixture(t, receipt)
	isCurrent, err := IsCurrent(receipt, current)
	if err != nil || !isCurrent {
		t.Fatalf("unchanged physical receipt current=%v err=%v; want current", isCurrent, err)
	}

	t.Run("exact_relevant_inputs_invalidate", func(t *testing.T) {
		mutations := []struct {
			name   string
			mutate func(*Currentness)
		}{
			{"source", func(value *Currentness) { value.SourceHashes[0] = proofSourceHash("changed-source") }},
			{"bundle", func(value *Currentness) { value.BundleHash = proofSourceHash("changed-bundle") }},
			{"schema", func(value *Currentness) { value.SchemaHash = proofSourceHash("changed-schema") }},
			{"assertion", func(value *Currentness) { value.AssertionHash = proofSemanticHash("changed-assertion") }},
			{"handler", func(value *Currentness) { value.HandlerVersions["http"] = "v2" }},
			{"operation", func(value *Currentness) { value.Identity.OperationID = "operation-v2" }},
			{"account_scope", func(value *Currentness) { value.Identity.ScopeHash = proofSemanticHash("changed-scope") }},
			{"client", func(value *Currentness) { value.PMClientHash = proofSourceHash("changed-client") }},
			{"api", func(value *Currentness) { value.PMAPIHash = proofSourceHash("changed-api") }},
			{"worker", func(value *Currentness) { value.PMWorkerHash = proofSourceHash("changed-worker") }},
			{"query_helper", func(value *Currentness) {
				hash := proofSourceHash("changed-query-helper")
				value.QueryHelperHash = &hash
			}},
			{"execution_kind", func(value *Currentness) { value.Identity.ExecutionKind = "fake" }},
			{"implementation_closure", func(value *Currentness) {
				digest := proofSemanticHash("changed-closure")
				value.ImplementationClosure.Digest = &digest
			}},
		}
		for _, test := range mutations {
			t.Run(test.name, func(t *testing.T) {
				changed := cloneCurrentness(current)
				test.mutate(&changed)
				matched, err := IsCurrent(receipt, changed)
				if err != nil {
					t.Fatalf("IsCurrent: %v", err)
				}
				if matched {
					t.Fatal("changed relevant identity reused an old pass")
				}
			})
		}
	})

	t.Run("unknown_or_absent_closure_invalidates", func(t *testing.T) {
		unknownCurrent := cloneCurrentness(current)
		unknownCurrent.ImplementationClosure = contract.ImplementationClosure{Known: false}
		matched, err := IsCurrent(receipt, unknownCurrent)
		if err != nil || matched {
			t.Fatalf("unknown current closure matched=%v err=%v", matched, err)
		}
		withoutClosure := cloneProofReceipt(t, receipt)
		withoutClosure.ImplementationClosure = nil
		matched, err = IsCurrent(withoutClosure, current)
		if err != nil || matched {
			t.Fatalf("absent receipt closure matched=%v err=%v", matched, err)
		}
	})

	t.Run("source_set_order_and_documentation_do_not_invalidate", func(t *testing.T) {
		reordered := cloneCurrentness(current)
		reordered.SourceHashes[0], reordered.SourceHashes[1] = reordered.SourceHashes[1], reordered.SourceHashes[0]
		matched, err := IsCurrent(receipt, reordered)
		if err != nil || !matched {
			t.Fatalf("same exact source set matched=%v err=%v", matched, err)
		}
		record := receiptRecordFixture(t, 7, contract.LaneEligible, receipt)
		key, err := CaseKey(current.Identity)
		if err != nil {
			t.Fatal(err)
		}
		decisions, err := SelectCases("", []CaseCandidate{{
			Current:      current,
			Availability: contract.LaneEligible,
			Reason:       "eligible after documentation edit",
		}}, map[string]ReceiptRecord{key: record})
		if err != nil {
			t.Fatal(err)
		}
		selected := decisions[0]
		if selected.Execute || !selected.Reused || selected.Receipt == nil ||
			selected.Receipt.Receipt.Provenance.Reused == nil ||
			*selected.Receipt.Receipt.Provenance.Reused ||
			selected.Receipt.Receipt.StartedAt == nil ||
			!selected.Receipt.Receipt.StartedAt.Equal(*receipt.StartedAt) ||
			len(selected.Receipt.Receipt.SourceHashes) != len(receipt.SourceHashes) ||
			selected.Receipt.Receipt.SourceHashes[0] != receipt.SourceHashes[0] ||
			selected.Receipt.Receipt.PMWorkerHash == nil ||
			*selected.Receipt.Receipt.PMWorkerHash != *receipt.PMWorkerHash {
			t.Fatalf("documentation-only reuse changed physical evidence: %#v", selected)
		}
	})

	t.Run("modes_and_availability", func(t *testing.T) {
		record := receiptRecordFixture(t, 1, contract.LaneEligible, receipt)
		key, err := CaseKey(current.Identity)
		if err != nil {
			t.Fatal(err)
		}
		candidate := CaseCandidate{Current: current, Availability: contract.LaneEligible, Reason: "eligible case"}
		retry, err := SelectCases(ModeRetry, []CaseCandidate{candidate}, map[string]ReceiptRecord{key: record})
		if err != nil || retry[0].Execute || !retry[0].Reused {
			t.Fatalf("retry selection = %#v err=%v", retry, err)
		}
		all, err := SelectCases(ModeAll, []CaseCandidate{candidate}, map[string]ReceiptRecord{key: record})
		if err != nil || !all[0].Execute || all[0].Reused {
			t.Fatalf("all selection = %#v err=%v", all, err)
		}
		unimplemented, err := SelectCases(ModeUnimplemented, []CaseCandidate{candidate}, map[string]ReceiptRecord{key: record})
		if err != nil || unimplemented[0].Execute || !unimplemented[0].Reused {
			t.Fatalf("unimplemented proven selection = %#v err=%v", unimplemented, err)
		}
		staleCandidate := candidate
		staleCandidate.Current = cloneCurrentness(candidate.Current)
		staleClosure := proofSemanticHash("stale-implementation-closure")
		staleCandidate.Current.ImplementationClosure.Digest = &staleClosure
		stale, err := SelectCases(ModeUnimplemented, []CaseCandidate{staleCandidate}, map[string]ReceiptRecord{key: record})
		if err != nil || stale[0].Execute || stale[0].Reused || stale[0].Receipt != nil {
			t.Fatalf("unimplemented stale pass selection = %#v err=%v", stale, err)
		}
		missing, err := SelectCases(ModeUnimplemented, []CaseCandidate{candidate}, nil)
		if err != nil || !missing[0].Execute {
			t.Fatalf("unimplemented missing selection = %#v err=%v", missing, err)
		}

		failed := failedProofReceipt(t, receipt, "ordinary-failure", started.Add(time.Hour))
		failedRecord := receiptRecordFixture(t, 2, contract.LaneEligible, failed)
		ordinaryFailure, err := SelectCases(ModeUnimplemented, []CaseCandidate{candidate}, map[string]ReceiptRecord{key: failedRecord})
		if err != nil || ordinaryFailure[0].Execute || ordinaryFailure[0].Reused {
			t.Fatalf("unimplemented ordinary failure selection = %#v err=%v", ordinaryFailure, err)
		}
		previouslyBlocked := receiptRecordFixture(t, 2, contract.LaneBlocked, failed)
		newlyEligible, err := SelectCases(ModeUnimplemented, []CaseCandidate{candidate}, map[string]ReceiptRecord{key: previouslyBlocked})
		if err != nil || !newlyEligible[0].Execute {
			t.Fatalf("newly eligible selection = %#v err=%v", newlyEligible, err)
		}

		unavailable := CaseCandidate{
			Current:      Currentness{Identity: current.Identity},
			Availability: contract.LaneUnsupported,
			Reason:       "operation has no supported implementation",
		}
		for _, mode := range []Mode{ModeRetry, ModeAll, ModeUnimplemented} {
			inventoried, err := SelectCases(mode, []CaseCandidate{unavailable}, nil)
			if err != nil || inventoried[0].Execute || inventoried[0].Reused || inventoried[0].Receipt != nil {
				t.Fatalf("%s unsupported inventory selection = %#v err=%v", mode, inventoried, err)
			}
		}
	})

	t.Run("invalid_selectors_and_foreign_records_fail_closed", func(t *testing.T) {
		if _, err := ParseMode("CURRENT"); err == nil {
			t.Fatal("CURRENT selector was accepted")
		}
		if _, err := ParseMode("invented"); err == nil {
			t.Fatal("unknown mode was accepted")
		}
		candidate := CaseCandidate{Current: current, Availability: contract.LaneEligible, Reason: "eligible case"}
		if _, err := SelectCases(ModeRetry, []CaseCandidate{candidate, candidate}, nil); err == nil {
			t.Fatal("duplicate case identities were accepted")
		}
		record := receiptRecordFixture(t, 1, contract.LaneEligible, receipt)
		if _, err := SelectCases(ModeRetry, []CaseCandidate{candidate}, map[string]ReceiptRecord{
			string(proofSemanticHash("foreign-key")): record,
		}); err == nil {
			t.Fatal("foreign latest receipt map entry was accepted")
		}
	})
}

func TestProofRunnerMechanics(t *testing.T) {
	t.Run("atomic_reports_preserve_checkpoints_and_independent_siblings", func(t *testing.T) {
		store, _ := openProofStore(t)
		base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
		firstReceipt := proofReceiptFixture(t, "case-first", base)
		siblingReceipt := proofReceiptFixture(t, "case-sibling", base.Add(time.Minute))
		first, err := store.Append(t.Context(), contract.LaneEligible, firstReceipt)
		if err != nil {
			t.Fatal(err)
		}
		sibling, err := store.Append(t.Context(), contract.LaneEligible, siblingReceipt)
		if err != nil {
			t.Fatal(err)
		}
		firstIdentity := currentnessFixture(t, firstReceipt).Identity
		siblingIdentity := currentnessFixture(t, siblingReceipt).Identity
		unavailableIdentity := firstIdentity
		unavailableIdentity.CaseID = "case-unavailable"
		initial := proofLaneReport([]ReportCase{
			{Identity: firstIdentity, Availability: contract.LaneEligible, Reason: "first case passed", ReceiptID: &first.ID},
			{Identity: siblingIdentity, Availability: contract.LaneEligible, Reason: "sibling case passed", ReceiptID: &sibling.ID},
			{Identity: unavailableIdentity, Availability: contract.LaneNotApplicable, Reason: "case is not applicable"},
		})
		if err := store.PublishReport(t.Context(), initial); err != nil {
			t.Fatalf("publish initial report: %v", err)
		}

		failureReceipt := failedProofReceipt(t, firstReceipt, "failed-first-case", base.Add(time.Hour))
		failure, err := store.Append(t.Context(), contract.LaneEligible, failureReceipt)
		if err != nil {
			t.Fatalf("append independent failure: %v", err)
		}
		staleReplacement := initial
		staleReplacement.GeneratedAt = initial.GeneratedAt.Add(time.Minute)
		if err := store.PublishReport(t.Context(), staleReplacement); err == nil {
			t.Fatal("replacement report accepted a non-latest receipt")
		}
		retained, found, err := store.ReadReport(t.Context(), initial.ConnectorID, initial.Lane)
		if err != nil || !found || !retained.GeneratedAt.Equal(initial.GeneratedAt) {
			t.Fatalf("previous complete report not retained: found=%v err=%v report=%#v", found, err, retained)
		}
		latestSibling, found, err := store.Latest(t.Context(), siblingIdentity)
		if err != nil || !found || latestSibling.ID != sibling.ID || latestSibling.Sequence != sibling.Sequence {
			t.Fatalf("successful sibling checkpoint changed: %#v found=%v err=%v", latestSibling, found, err)
		}
		latestFailure, found, err := store.Latest(t.Context(), firstIdentity)
		if err != nil || !found || latestFailure.ID != failure.ID {
			t.Fatalf("completed failed checkpoint lost: %#v found=%v err=%v", latestFailure, found, err)
		}

		replacement := proofLaneReport([]ReportCase{
			{Identity: firstIdentity, Availability: contract.LaneEligible, Reason: "first case failed", ReceiptID: &failure.ID},
			{Identity: siblingIdentity, Availability: contract.LaneEligible, Reason: "sibling pass reused", ReceiptID: &sibling.ID, Reused: true},
			{Identity: unavailableIdentity, Availability: contract.LaneNotApplicable, Reason: "case is not applicable"},
		})
		replacement.GeneratedAt = initial.GeneratedAt.Add(2 * time.Minute)
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		if err := store.PublishReport(cancelled, replacement); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled replacement error = %v", err)
		}
		retained, found, err = store.ReadReport(t.Context(), initial.ConnectorID, initial.Lane)
		if err != nil || !found || !retained.GeneratedAt.Equal(initial.GeneratedAt) {
			t.Fatalf("cancelled assembly replaced prior report: found=%v err=%v report=%#v", found, err, retained)
		}
		if err := store.PublishReport(t.Context(), replacement); err != nil {
			t.Fatalf("publish complete replacement: %v", err)
		}
		readBack, found, err := store.ReadReport(t.Context(), replacement.ConnectorID, replacement.Lane)
		if err != nil || !found || !readBack.GeneratedAt.Equal(replacement.GeneratedAt) || len(readBack.Cases) != 3 {
			t.Fatalf("replacement report = %#v found=%v err=%v", readBack, found, err)
		}

		firstKey, _ := CaseKey(firstIdentity)
		siblingKey, _ := CaseKey(siblingIdentity)
		decisions, err := SelectCases(ModeRetry, []CaseCandidate{
			{Current: currentnessFixture(t, firstReceipt), Availability: contract.LaneEligible, Reason: "first eligible"},
			{Current: currentnessFixture(t, siblingReceipt), Availability: contract.LaneEligible, Reason: "sibling eligible"},
		}, map[string]ReceiptRecord{firstKey: latestFailure, siblingKey: latestSibling})
		if err != nil || len(decisions) != 2 || !decisions[0].Execute || decisions[1].Execute || !decisions[1].Reused {
			t.Fatalf("independent sibling decisions = %#v err=%v", decisions, err)
		}
	})

	t.Run("case_lock_bounds_cancellation_without_blocking_checkpoints", func(t *testing.T) {
		store, _ := openProofStore(t)
		base := time.Date(2026, 9, 19, 13, 0, 0, 0, time.UTC)
		receipt := proofReceiptFixture(t, "case-locked", base)
		identity := currentnessFixture(t, receipt).Identity
		release, err := store.LockCase(t.Context(), identity)
		if err != nil {
			t.Fatalf("lock case: %v", err)
		}
		if _, err := store.Append(t.Context(), contract.LaneEligible, receipt); err != nil {
			t.Fatalf("append while case execution lock held: %v", err)
		}
		if _, found, err := store.Latest(t.Context(), identity); err != nil || !found {
			t.Fatalf("latest while case execution lock held: found=%v err=%v", found, err)
		}
		otherReceipt := proofReceiptFixture(t, "case-other-lock", base)
		otherRelease, err := store.LockCase(t.Context(), currentnessFixture(t, otherReceipt).Identity)
		if err != nil {
			t.Fatalf("independent case lock: %v", err)
		}
		if err := otherRelease(); err != nil {
			t.Fatalf("release independent case lock: %v", err)
		}

		waiting, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if _, err := store.LockCase(waiting, identity); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("contended case lock error = %v; want deadline", err)
		}
		if err := release(); err != nil {
			t.Fatalf("release case lock: %v", err)
		}
		if err := release(); err != nil {
			t.Fatalf("idempotent case lock release: %v", err)
		}
		reacquired, err := store.LockCase(t.Context(), identity)
		if err != nil {
			t.Fatalf("reacquire released case: %v", err)
		}
		if err := reacquired(); err != nil {
			t.Fatalf("release reacquired case: %v", err)
		}
	})

	t.Run("receipt_serialization_lock_honors_cancellation", func(t *testing.T) {
		store, _ := openProofStore(t)
		receipt := proofReceiptFixture(t, "case-cancelled-append", time.Date(2026, 9, 19, 13, 30, 0, 0, time.UTC))
		if err := acquireProofStoreLock(t.Context(), store.lock); err != nil {
			t.Fatalf("hold receipt serialization lock: %v", err)
		}
		waiting, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		_, appendErr := store.Append(waiting, contract.LaneEligible, receipt)
		cancel()
		releaseProofStoreLock(store.lock)
		if !errors.Is(appendErr, context.DeadlineExceeded) {
			t.Fatalf("contended append error = %v; want deadline", appendErr)
		}
		identity := currentnessFixture(t, receipt).Identity
		if _, found, err := store.Latest(t.Context(), identity); err != nil || found {
			t.Fatalf("cancelled append left a checkpoint: found=%v err=%v", found, err)
		}
		if _, err := store.Append(t.Context(), contract.LaneEligible, receipt); err != nil {
			t.Fatalf("append after releasing serialization lock: %v", err)
		}
	})

	t.Run("concurrent_writers_serialize_sequences_without_history_truncation", func(t *testing.T) {
		store, _ := openProofStore(t)
		base := time.Date(2026, 9, 19, 14, 0, 0, 0, time.UTC)
		const writerCount = 12
		receipts := make([]contract.ProofReceipt, writerCount)
		for index := range receipts {
			passing := proofReceiptFixture(t, "case-concurrent", base.Add(time.Duration(index)*time.Minute))
			receipts[index] = failedProofReceipt(t, passing, fmt.Sprintf("failed-run-%d", index), base.Add(time.Duration(index)*time.Minute))
		}
		records := make(chan ReceiptRecord, writerCount)
		errorsFound := make(chan error, writerCount)
		var writers sync.WaitGroup
		for _, receipt := range receipts {
			writers.Add(1)
			go func() {
				defer writers.Done()
				record, err := store.Append(t.Context(), contract.LaneEligible, receipt)
				if err != nil {
					errorsFound <- err
					return
				}
				records <- record
			}()
		}
		writers.Wait()
		close(records)
		close(errorsFound)
		for err := range errorsFound {
			t.Fatalf("concurrent append: %v", err)
		}
		sequences := make(map[uint64]struct{}, writerCount)
		for record := range records {
			sequences[record.Sequence] = struct{}{}
		}
		if len(sequences) != writerCount {
			t.Fatalf("persisted sequence count = %d; want %d", len(sequences), writerCount)
		}
		for sequence := uint64(1); sequence <= writerCount; sequence++ {
			if _, present := sequences[sequence]; !present {
				t.Fatalf("persisted sequence %d is missing", sequence)
			}
		}
		identity := currentnessFixture(t, receipts[0]).Identity
		latest, found, err := store.Latest(t.Context(), identity)
		if err != nil || !found || latest.Sequence != writerCount {
			t.Fatalf("latest concurrent receipt = %#v found=%v err=%v", latest, found, err)
		}
	})

	t.Run("unsafe_paths_members_and_corruption_fail_closed", func(t *testing.T) {
		if _, err := OpenReceiptStore("relative-proof-store", 1<<20); err == nil {
			t.Fatal("relative proof store path was accepted")
		}
		target := t.TempDir()
		rootParent := t.TempDir()
		rootLink := filepath.Join(rootParent, "proof-store-link")
		if err := os.Symlink(target, rootLink); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenReceiptStore(rootLink, 1<<20); err == nil {
			t.Fatal("symlink proof store root was accepted")
		}

		store, directory := openProofStore(t)
		base := time.Date(2026, 9, 19, 15, 0, 0, 0, time.UTC)
		receipt := proofReceiptFixture(t, "case-hostile-member", base)
		record, err := store.Append(t.Context(), contract.LaneEligible, receipt)
		if err != nil {
			t.Fatal(err)
		}
		identity := currentnessFixture(t, receipt).Identity
		key, _ := CaseKey(identity)
		hostileName, _ := receiptFileName(key, record.Sequence+1)
		hostilePath := filepath.Join(directory, hostileName)
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, hostilePath); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Latest(t.Context(), identity); err == nil {
			t.Fatal("symlink receipt member was followed or ignored")
		}
		if err := os.Remove(hostilePath); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(hostilePath, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Latest(t.Context(), identity); err == nil {
			t.Fatal("FIFO receipt member was accepted")
		}
		if err := os.Remove(hostilePath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(hostilePath, []byte(`{"corrupt":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Latest(t.Context(), identity); err == nil {
			t.Fatal("corrupt receipt member was accepted")
		}
		if err := os.Remove(hostilePath); err != nil {
			t.Fatal(err)
		}
		latest, found, err := store.Latest(t.Context(), identity)
		if err != nil || !found || latest.ID != record.ID {
			t.Fatalf("valid completed receipt was lost after hostile members: %#v found=%v err=%v", latest, found, err)
		}

		caseName, _ := caseLockFileName(key)
		casePath := filepath.Join(directory, caseName)
		if err := os.Symlink(outside, casePath); err != nil {
			t.Fatal(err)
		}
		if _, err := store.LockCase(t.Context(), identity); err == nil {
			t.Fatal("symlink case lock was followed")
		}
		if err := os.Remove(casePath); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(casePath, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.LockCase(t.Context(), identity); err == nil {
			t.Fatal("FIFO case lock was accepted")
		}
	})

	t.Run("report_publication_refuses_symlink_destination", func(t *testing.T) {
		store, directory := openProofStore(t)
		identity := currentnessFixture(t, proofReceiptFixture(t, "case-report-unavailable", time.Date(2026, 9, 19, 16, 0, 0, 0, time.UTC))).Identity
		report := proofLaneReport([]ReportCase{{
			Identity:     identity,
			Availability: contract.LaneUnsupported,
			Reason:       "handler is unavailable",
		}})
		name, _ := reportFileName(report.ConnectorID, report.Lane)
		outside := filepath.Join(t.TempDir(), "outside-report")
		if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
		if err := store.PublishReport(t.Context(), report); err == nil {
			t.Fatal("report publication replaced a symlink destination")
		}
		value, err := os.ReadFile(outside)
		if err != nil || string(value) != "unchanged" {
			t.Fatalf("outside report target changed: %q err=%v", value, err)
		}
	})
}

func openProofStore(t *testing.T) (*ReceiptStore, string) {
	t.Helper()
	directory := t.TempDir()
	store, err := OpenReceiptStore(directory, 1<<20)
	if err != nil {
		t.Fatalf("open proof receipt store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close proof receipt store: %v", err)
		}
	})
	return store, directory
}

func proofReceiptFixture(t *testing.T, caseID string, started time.Time) contract.ProofReceipt {
	t.Helper()
	runID := "run-" + caseID + "-" + fmt.Sprint(started.Unix())
	hostClass := "controlled-darwin-arm64"
	reused := false
	passed := true
	bundle := proofSourceHash("bundle")
	schema := proofSourceHash("schema")
	client := proofSourceHash("pm-client")
	api := proofSourceHash("pm-api")
	worker := proofSourceHash("pm-worker")
	query := proofSourceHash("query-helper")
	scope := proofSemanticHash("workspace-alpha-account-controlled")
	closure := proofSemanticHash("implementation-closure")
	finished := started.Add(2 * time.Second)
	receipt := contract.ProofReceipt{
		ProofVersion:              contract.ProofSchemaVersion,
		ConnectorID:               "github",
		Lane:                      contract.LaneDirectRead,
		OperationID:               "issues-list",
		BehaviorID:                "complete-record",
		CaseID:                    caseID,
		RunID:                     &runID,
		SourceHashes:              []contract.SourceHash{proofSourceHash("source-openapi"), proofSourceHash("source-semantics")},
		BundleHash:                &bundle,
		SchemaHash:                &schema,
		AssertionHash:             proofSemanticHash("assertion-set"),
		PMClientHash:              &client,
		PMAPIHash:                 &api,
		PMWorkerHash:              &worker,
		QueryHelperHashIfUsed:     &query,
		HandlerVersions:           map[string]string{"http": "v1"},
		WorkspaceAccountScopeHash: &scope,
		Command:                   []string{"pm", "proof", caseID},
		Request: &contract.ProofRequestObservation{
			Method:          "GET",
			Target:          "https://api.example.test/issues",
			MediaType:       "application/json",
			SanitizedFields: map[string]json.RawMessage{"page": json.RawMessage(`1`)},
			Redactions:      []contract.ProofRedaction{},
		},
		Response: &contract.ProofResponseObservation{
			Status:          200,
			MediaType:       "application/json",
			SanitizedFields: map[string]json.RawMessage{"record": json.RawMessage(`{"id":"record-1"}`)},
			Redactions:      []contract.ProofRedaction{},
		},
		Assertions: []contract.ProofAssertionObservation{{
			AssertionID: "record-equals",
			Expected:    json.RawMessage(`{"id":"record-1"}`),
			Observed:    json.RawMessage(`{"id":"record-1"}`),
			Passed:      &passed,
		}},
		Effect: contract.ProofEffect{
			State:           contract.OutcomeSucceeded,
			ReceiptRefs:     []string{"provider-receipt-1"},
			OwnedObjectRefs: []string{},
		},
		Status:     contract.ProofPassedLive,
		StartedAt:  &started,
		FinishedAt: &finished,
		Provenance: contract.ProofProvenance{
			ExecutionKind: "live",
			HostClass:     &hostClass,
			Reused:        &reused,
		},
		ImplementationClosure: &contract.ImplementationClosure{Known: true, Digest: &closure},
		CleanupDiagnostics:    []contract.ProofCleanupDiagnostic{},
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("proof receipt fixture: %v", err)
	}
	return receipt
}

func failedProofReceipt(t *testing.T, original contract.ProofReceipt, runID string, started time.Time) contract.ProofReceipt {
	t.Helper()
	failed := cloneProofReceipt(t, original)
	passed := false
	finished := started.Add(time.Second)
	failed.RunID = &runID
	failed.Assertions[0].Passed = &passed
	failed.Effect.State = contract.OutcomeRejected
	failed.Status = contract.ProofFailed
	failed.StartedAt = &started
	failed.FinishedAt = &finished
	if err := failed.Validate(); err != nil {
		t.Fatalf("failed proof receipt fixture: %v", err)
	}
	return failed
}

func cloneProofReceipt(t *testing.T, receipt contract.ProofReceipt) contract.ProofReceipt {
	t.Helper()
	document, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal proof receipt clone: %v", err)
	}
	var cloned contract.ProofReceipt
	if err := json.Unmarshal(document, &cloned); err != nil {
		t.Fatalf("unmarshal proof receipt clone: %v", err)
	}
	return cloned
}

func currentnessFixture(t *testing.T, receipt contract.ProofReceipt) Currentness {
	t.Helper()
	identity, err := receiptCaseIdentity(receipt)
	if err != nil {
		t.Fatalf("derive proof case identity: %v", err)
	}
	if receipt.BundleHash == nil || receipt.SchemaHash == nil || receipt.PMClientHash == nil ||
		receipt.PMAPIHash == nil || receipt.PMWorkerHash == nil || receipt.ImplementationClosure == nil {
		t.Fatal("receipt fixture lacks currentness fields")
	}
	return Currentness{
		Identity:              identity,
		SourceHashes:          append([]contract.SourceHash(nil), receipt.SourceHashes...),
		BundleHash:            *receipt.BundleHash,
		SchemaHash:            *receipt.SchemaHash,
		PMClientHash:          *receipt.PMClientHash,
		PMAPIHash:             *receipt.PMAPIHash,
		PMWorkerHash:          *receipt.PMWorkerHash,
		AssertionHash:         receipt.AssertionHash,
		QueryHelperHash:       cloneSourceHashPointer(receipt.QueryHelperHashIfUsed),
		HandlerVersions:       cloneStringMap(receipt.HandlerVersions),
		ImplementationClosure: cloneImplementationClosure(*receipt.ImplementationClosure),
	}
}

func cloneCurrentness(value Currentness) Currentness {
	value.SourceHashes = append([]contract.SourceHash(nil), value.SourceHashes...)
	value.QueryHelperHash = cloneSourceHashPointer(value.QueryHelperHash)
	value.HandlerVersions = cloneStringMap(value.HandlerVersions)
	value.ImplementationClosure = cloneImplementationClosure(value.ImplementationClosure)
	return value
}

func cloneImplementationClosure(value contract.ImplementationClosure) contract.ImplementationClosure {
	if value.Digest != nil {
		digest := *value.Digest
		value.Digest = &digest
	}
	return value
}

func cloneSourceHashPointer(value *contract.SourceHash) *contract.SourceHash {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneStringMap(value map[string]string) map[string]string {
	cloned := make(map[string]string, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

func receiptRecordFixture(t *testing.T, sequence uint64, availability contract.LaneStatus, receipt contract.ProofReceipt) ReceiptRecord {
	t.Helper()
	id, err := proofReceiptDigest(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return ReceiptRecord{Sequence: sequence, ID: id, Availability: availability, Receipt: receipt}
}

func proofLaneReport(cases []ReportCase) LaneReport {
	return LaneReport{
		SchemaVersion: ReportSchemaVersion,
		ConnectorID:   "github",
		Lane:          contract.LaneDirectRead,
		InventoryHash: proofSemanticHash("inventory"),
		GeneratedAt:   time.Date(2026, 9, 19, 17, 0, 0, 0, time.UTC),
		Cases:         cases,
	}
}

func proofSourceHash(label string) contract.SourceHash {
	return contract.RawSourceHash([]byte(label))
}

func proofSemanticHash(label string) contract.SemanticDigest {
	return contract.SemanticDigest(contract.RawSourceHash([]byte(label)))
}
