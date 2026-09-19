package proof

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"golang.org/x/sys/unix"
)

// ReportSchemaVersion identifies complete derived proof lane reports.
const ReportSchemaVersion = "pm.proof-report/v1"

const (
	receiptRecordSchemaVersion = "pm.proof-receipt-record/v1"
	proofStoreLockName         = ".proof-store.lock"
	directoryReadBatch         = 128
	lockRetryDelay             = 10 * time.Millisecond
	maximumStoredJSONDepth     = 128
)

type LaneReport struct {
	SchemaVersion string                  `json:"schema_version"`
	ConnectorID   string                  `json:"connector_id"`
	Lane          contract.Lane           `json:"lane"`
	InventoryHash contract.SemanticDigest `json:"inventory_hash"`
	GeneratedAt   time.Time               `json:"generated_at"`
	Cases         []ReportCase            `json:"cases"`
}

type ReportCase struct {
	Identity     CaseIdentity             `json:"identity"`
	Availability contract.LaneStatus      `json:"availability"`
	Reason       string                   `json:"reason"`
	ReceiptID    *contract.SemanticDigest `json:"receipt_id"`
	Reused       bool                     `json:"reused"`
}

type receiptDocument struct {
	SchemaVersion string                  `json:"schema_version"`
	CaseKey       string                  `json:"case_key"`
	Sequence      uint64                  `json:"sequence"`
	ID            contract.SemanticDigest `json:"id"`
	Availability  contract.LaneStatus     `json:"availability"`
	Receipt       contract.ProofReceipt   `json:"receipt"`
}

type inProcessCaseLock struct {
	token      chan struct{}
	references int
}

type ReceiptStore struct {
	mu                   sync.RWMutex
	root                 *os.File
	lock                 *os.File
	rootIdentity         string
	maximumDocumentBytes int64
	closed               bool
	// caseLocks serializes one case inside this process; the cross-process
	// guarantee is the descriptor lock below.
	caseLocks struct {
		sync.Mutex
		entries map[string]*inProcessCaseLock
	}
}

// ErrReportStale reports a retained report whose receipts are no longer the
// latest for their cases, so it cannot stand for current evidence.
var ErrReportStale = errors.New("proof lane report is stale")

// proofStoreProcessFence serializes descriptor locking inside one process: an OS
// file lock is held per process, so two stores in one process would otherwise
// both observe it as acquired.
var proofStoreProcessFence = make(chan struct{}, 1)

func OpenReceiptStore(directory string, maximumDocumentBytes int64) (*ReceiptStore, error) {
	if directory == "" || strings.ContainsRune(directory, '\x00') || !filepath.IsAbs(directory) {
		return nil, fmt.Errorf("proof receipt directory must be an absolute path")
	}
	if maximumDocumentBytes <= 0 || maximumDocumentBytes == math.MaxInt64 {
		return nil, fmt.Errorf("proof receipt document bound must be positive")
	}
	cleanDirectory := filepath.Clean(directory)
	if err := os.MkdirAll(cleanDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create proof receipt directory: %w", err)
	}
	information, err := os.Lstat(cleanDirectory)
	if err != nil || information.Mode()&os.ModeSymlink != 0 || !information.IsDir() {
		return nil, fmt.Errorf("proof receipt directory is not a real directory")
	}

	rootFD, err := unix.Open(cleanDirectory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open proof receipt directory: %w", err)
	}
	root := os.NewFile(uintptr(rootFD), cleanDirectory)
	if root == nil {
		_ = unix.Close(rootFD)
		return nil, fmt.Errorf("open proof receipt directory descriptor")
	}
	closeRoot := true
	defer func() {
		if closeRoot {
			_ = root.Close()
		}
	}()
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil || rootStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("proof receipt root descriptor is not a directory")
	}

	lockFD, err := unix.Openat(rootFD, proofStoreLockName,
		unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open proof receipt process lock: %w", err)
	}
	lock := os.NewFile(uintptr(lockFD), proofStoreLockName)
	if lock == nil {
		_ = unix.Close(lockFD)
		return nil, fmt.Errorf("open proof receipt process lock descriptor")
	}
	closeLock := true
	defer func() {
		if closeLock {
			_ = lock.Close()
		}
	}()
	var lockStat unix.Stat_t
	if err := unix.Fstat(lockFD, &lockStat); err != nil ||
		lockStat.Mode&unix.S_IFMT != unix.S_IFREG || lockStat.Nlink != 1 {
		return nil, fmt.Errorf("proof receipt process lock is not a private regular file")
	}
	if err := unix.Fsync(rootFD); err != nil {
		return nil, fmt.Errorf("synchronize proof receipt directory: %w", err)
	}

	closeRoot = false
	closeLock = false
	return &ReceiptStore{
		root:                 root,
		lock:                 lock,
		rootIdentity:         fmt.Sprintf("%d:%d", rootStat.Dev, rootStat.Ino),
		maximumDocumentBytes: maximumDocumentBytes,
	}, nil
}

func (store *ReceiptStore) Close() error {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	var lockErr, rootErr error
	if store.lock != nil {
		lockErr = store.lock.Close()
		store.lock = nil
	}
	if store.root != nil {
		rootErr = store.root.Close()
		store.root = nil
	}
	return errors.Join(lockErr, rootErr)
}

func (store *ReceiptStore) Append(ctx context.Context, availability contract.LaneStatus, receipt contract.ProofReceipt) (ReceiptRecord, error) {
	if !validLaneStatus(availability) {
		return ReceiptRecord{}, fmt.Errorf("proof receipt availability is unsupported")
	}
	if receipt.Provenance.Reused != nil && *receipt.Provenance.Reused {
		return ReceiptRecord{}, fmt.Errorf("physical proof receipt cannot be marked reused")
	}
	id, err := proofReceiptDigest(receipt)
	if err != nil {
		return ReceiptRecord{}, err
	}
	if proofPassed(receipt.Status) && availability != contract.LaneEligible {
		return ReceiptRecord{}, fmt.Errorf("unavailable proof case cannot publish a passing receipt")
	}
	identity, err := receiptCaseIdentity(receipt)
	if err != nil {
		return ReceiptRecord{}, err
	}
	key, err := CaseKey(identity)
	if err != nil {
		return ReceiptRecord{}, err
	}

	rootFD, release, err := store.begin(ctx)
	if err != nil {
		return ReceiptRecord{}, err
	}
	defer release()

	latest, found, err := store.latestRecordLocked(ctx, rootFD, key)
	if err != nil {
		return ReceiptRecord{}, err
	}
	sequence := uint64(1)
	if found {
		if latest.Sequence == math.MaxUint64 {
			return ReceiptRecord{}, fmt.Errorf("proof receipt sequence is exhausted")
		}
		sequence = latest.Sequence + 1
	}
	record := ReceiptRecord{Sequence: sequence, ID: id, Availability: availability, Receipt: receipt}
	document := receiptDocument{
		SchemaVersion: receiptRecordSchemaVersion,
		CaseKey:       key,
		Sequence:      sequence,
		ID:            id,
		Availability:  availability,
		Receipt:       receipt,
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return ReceiptRecord{}, fmt.Errorf("encode proof receipt record: %w", err)
	}
	if int64(len(encoded)) > store.maximumDocumentBytes {
		return ReceiptRecord{}, fmt.Errorf("proof receipt record exceeds its document bound")
	}
	name, err := receiptFileName(key, sequence)
	if err != nil {
		return ReceiptRecord{}, err
	}
	if err := store.publishImmutableLocked(ctx, rootFD, name, encoded); err != nil {
		return ReceiptRecord{}, err
	}
	return record, nil
}

func (store *ReceiptStore) Latest(ctx context.Context, identity CaseIdentity) (ReceiptRecord, bool, error) {
	key, err := CaseKey(identity)
	if err != nil {
		return ReceiptRecord{}, false, err
	}
	rootFD, release, err := store.begin(ctx)
	if err != nil {
		return ReceiptRecord{}, false, err
	}
	defer release()
	return store.latestRecordLocked(ctx, rootFD, key)
}

// LockCase excludes another physical execution of the exact case across
// goroutines and processes. It deliberately uses a separate lock from receipt
// serialization, so the owner can re-read Latest and append its checkpoint
// while holding the returned release function.
func (store *ReceiptStore) LockCase(ctx context.Context, identity CaseIdentity) (func() error, error) {
	if store == nil || ctx == nil {
		return nil, fmt.Errorf("proof receipt store and context are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := CaseKey(identity)
	if err != nil {
		return nil, err
	}
	name, err := caseLockFileName(key)
	if err != nil {
		return nil, err
	}

	store.mu.RLock()
	if store.closed || store.root == nil || store.rootIdentity == "" {
		store.mu.RUnlock()
		return nil, fmt.Errorf("proof receipt store is closed")
	}
	processRelease, err := store.acquireInProcessCaseLock(ctx, store.rootIdentity+"\x00"+key)
	if err != nil {
		store.mu.RUnlock()
		return nil, err
	}
	rootFD := int(store.root.Fd())
	fd, err := unix.Openat(rootFD, name,
		unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		processRelease()
		store.mu.RUnlock()
		return nil, fmt.Errorf("open proof case execution lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		processRelease()
		store.mu.RUnlock()
		return nil, fmt.Errorf("open proof case execution lock descriptor")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		_ = file.Close()
		processRelease()
		store.mu.RUnlock()
		return nil, fmt.Errorf("proof case execution lock is not a private regular file")
	}
	if err := acquireDescriptorLock(ctx, fd); err != nil {
		_ = file.Close()
		processRelease()
		store.mu.RUnlock()
		return nil, err
	}
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()
		processRelease()
		store.mu.RUnlock()
		return nil, fmt.Errorf("proof case execution lock changed identity")
	}
	if err := ctx.Err(); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()
		processRelease()
		store.mu.RUnlock()
		return nil, err
	}

	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			unlockErr := unix.Flock(fd, unix.LOCK_UN)
			closeErr := file.Close()
			processRelease()
			releaseErr = errors.Join(unlockErr, closeErr)
		})
		return releaseErr
	}
	store.mu.RUnlock()
	return release, nil
}

func (store *ReceiptStore) PublishReport(ctx context.Context, report LaneReport) error {
	if err := validateLaneReportStructure(report); err != nil {
		return err
	}
	rootFD, release, err := store.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := store.validateReportReferencesLocked(ctx, rootFD, report, true); err != nil {
		return err
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode proof lane report: %w", err)
	}
	if int64(len(encoded)) > store.maximumDocumentBytes {
		return fmt.Errorf("proof lane report exceeds its document bound")
	}
	name, err := reportFileName(report.ConnectorID, report.Lane)
	if err != nil {
		return err
	}
	return store.replaceAtomicallyLocked(ctx, rootFD, name, encoded)
}

// ReadReport returns the retained report for one connector lane together with
// its exact immutable receipt references. A retained report is historical
// evidence: a newer relevant failure does not rewrite or delete it, so callers
// that need current evidence must use ReadCurrentReport.
func (store *ReceiptStore) ReadReport(ctx context.Context, connectorID string, lane contract.Lane) (LaneReport, bool, error) {
	name, err := reportFileName(connectorID, lane)
	if err != nil {
		return LaneReport{}, false, err
	}
	rootFD, release, err := store.begin(ctx)
	if err != nil {
		return LaneReport{}, false, err
	}
	defer release()
	document, err := readBoundedRegularFile(rootFD, name, store.maximumDocumentBytes)
	if errors.Is(err, unix.ENOENT) {
		return LaneReport{}, false, nil
	}
	if err != nil {
		return LaneReport{}, false, fmt.Errorf("read proof lane report: %w", err)
	}
	var report LaneReport
	if err := decodeStoredJSON(document, &report); err != nil {
		return LaneReport{}, false, fmt.Errorf("decode proof lane report: %w", err)
	}
	if report.ConnectorID != connectorID || report.Lane != lane {
		return LaneReport{}, false, fmt.Errorf("proof lane report selector does not match its contents")
	}
	if err := validateLaneReportStructure(report); err != nil {
		return LaneReport{}, false, err
	}
	if err := store.validateReportReferencesLocked(ctx, rootFD, report, false); err != nil {
		return LaneReport{}, false, err
	}
	return report, true, nil
}

// ReadCurrentReport returns the retained report only while every case still
// references the latest immutable receipt. A report that a newer failure or
// invalidation has overtaken is refused with ErrReportStale instead of being
// presented as current evidence.
func (store *ReceiptStore) ReadCurrentReport(ctx context.Context, connectorID string, lane contract.Lane) (LaneReport, bool, error) {
	report, found, err := store.ReadReport(ctx, connectorID, lane)
	if err != nil || !found {
		return report, found, err
	}
	rootFD, release, err := store.begin(ctx)
	if err != nil {
		return LaneReport{}, false, err
	}
	defer release()
	if err := store.validateReportReferencesLocked(ctx, rootFD, report, true); err != nil {
		return LaneReport{}, false, errors.Join(ErrReportStale, err)
	}
	return report, true, nil
}

func (store *ReceiptStore) begin(ctx context.Context) (int, func(), error) {
	if store == nil || ctx == nil {
		return -1, nil, fmt.Errorf("proof receipt store and context are required")
	}
	if err := ctx.Err(); err != nil {
		return -1, nil, err
	}
	store.mu.RLock()
	if store.closed || store.root == nil || store.lock == nil {
		store.mu.RUnlock()
		return -1, nil, fmt.Errorf("proof receipt store is closed")
	}
	if err := acquireProofStoreLock(ctx, store.lock); err != nil {
		store.mu.RUnlock()
		return -1, nil, err
	}
	return int(store.root.Fd()), func() {
		releaseProofStoreLock(store.lock)
		store.mu.RUnlock()
	}, nil
}

func acquireProofStoreLock(ctx context.Context, lock *os.File) error {
	select {
	case proofStoreProcessFence <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	releaseFence := true
	defer func() {
		if releaseFence {
			<-proofStoreProcessFence
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			var stat unix.Stat_t
			if err := unix.Fstat(int(lock.Fd()), &stat); err != nil ||
				stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
				_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
				return fmt.Errorf("proof receipt process lock changed identity")
			}
			if err := ctx.Err(); err != nil {
				_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
				return err
			}
			releaseFence = false
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("acquire proof receipt process lock: %w", err)
		}
		timer := time.NewTimer(lockRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func releaseProofStoreLock(lock *os.File) {
	_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	<-proofStoreProcessFence
}

func acquireDescriptorLock(ctx context.Context, fd int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("acquire proof case execution lock: %w", err)
		}
		timer := time.NewTimer(lockRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (store *ReceiptStore) acquireInProcessCaseLock(ctx context.Context, key string) (func(), error) {
	store.caseLocks.Lock()
	if store.caseLocks.entries == nil {
		store.caseLocks.entries = make(map[string]*inProcessCaseLock)
	}
	entry := store.caseLocks.entries[key]
	if entry == nil {
		entry = &inProcessCaseLock{token: make(chan struct{}, 1)}
		store.caseLocks.entries[key] = entry
	}
	entry.references++
	store.caseLocks.Unlock()

	select {
	case entry.token <- struct{}{}:
		return func() {
			<-entry.token
			store.releaseInProcessCaseLockReference(key, entry)
		}, nil
	case <-ctx.Done():
		store.releaseInProcessCaseLockReference(key, entry)
		return nil, ctx.Err()
	}
}

func (store *ReceiptStore) releaseInProcessCaseLockReference(key string, entry *inProcessCaseLock) {
	store.caseLocks.Lock()
	defer store.caseLocks.Unlock()
	entry.references--
	if entry.references == 0 && store.caseLocks.entries[key] == entry {
		delete(store.caseLocks.entries, key)
	}
}

func (store *ReceiptStore) latestRecordLocked(ctx context.Context, rootFD int, key string) (ReceiptRecord, bool, error) {
	keyHex, err := semanticDigestHex(contract.SemanticDigest(key))
	if err != nil {
		return ReceiptRecord{}, false, err
	}
	prefix := "receipt-" + keyHex + "-"
	var latest ReceiptRecord
	found := false
	err = forEachRootEntry(ctx, rootFD, func(name string) error {
		if !strings.HasPrefix(name, prefix) {
			return nil
		}
		sequence, err := parseReceiptFileName(name, prefix)
		if err != nil {
			return err
		}
		record, err := store.readReceiptRecordLocked(rootFD, name, key, sequence)
		if err != nil {
			return err
		}
		if !found || record.Sequence > latest.Sequence {
			latest = record
			found = true
		}
		return nil
	})
	if err != nil {
		return ReceiptRecord{}, false, err
	}
	return latest, found, nil
}

func (store *ReceiptStore) findReceiptRecordLocked(
	ctx context.Context,
	rootFD int,
	identity CaseIdentity,
	id contract.SemanticDigest,
	availability contract.LaneStatus,
) (ReceiptRecord, bool, error) {
	key, err := CaseKey(identity)
	if err != nil {
		return ReceiptRecord{}, false, err
	}
	keyHex, err := semanticDigestHex(contract.SemanticDigest(key))
	if err != nil {
		return ReceiptRecord{}, false, err
	}
	prefix := "receipt-" + keyHex + "-"
	var matched ReceiptRecord
	found := false
	err = forEachRootEntry(ctx, rootFD, func(name string) error {
		if !strings.HasPrefix(name, prefix) {
			return nil
		}
		sequence, err := parseReceiptFileName(name, prefix)
		if err != nil {
			return err
		}
		record, err := store.readReceiptRecordLocked(rootFD, name, key, sequence)
		if err != nil {
			return err
		}
		if record.ID == id && record.Availability == availability &&
			(!found || record.Sequence > matched.Sequence) {
			matched = record
			found = true
		}
		return nil
	})
	if err != nil {
		return ReceiptRecord{}, false, err
	}
	return matched, found, nil
}

func (store *ReceiptStore) readReceiptRecordLocked(
	rootFD int,
	name string,
	key string,
	sequence uint64,
) (ReceiptRecord, error) {
	document, err := readBoundedRegularFile(rootFD, name, store.maximumDocumentBytes)
	if err != nil {
		return ReceiptRecord{}, fmt.Errorf("read immutable proof receipt: %w", err)
	}
	var stored receiptDocument
	if err := decodeStoredJSON(document, &stored); err != nil {
		return ReceiptRecord{}, fmt.Errorf("decode immutable proof receipt: %w", err)
	}
	if stored.SchemaVersion != receiptRecordSchemaVersion || stored.CaseKey != key ||
		stored.Sequence != sequence {
		return ReceiptRecord{}, fmt.Errorf("immutable proof receipt metadata does not match its file identity")
	}
	record := ReceiptRecord{
		Sequence:     stored.Sequence,
		ID:           stored.ID,
		Availability: stored.Availability,
		Receipt:      stored.Receipt,
	}
	if err := validateReceiptRecord(record); err != nil {
		return ReceiptRecord{}, fmt.Errorf("validate immutable proof receipt: %w", err)
	}
	identity, err := receiptCaseIdentity(record.Receipt)
	if err != nil {
		return ReceiptRecord{}, err
	}
	recordKey, err := CaseKey(identity)
	if err != nil || recordKey != key {
		return ReceiptRecord{}, fmt.Errorf("immutable proof receipt has a foreign case identity")
	}
	return record, nil
}

func (store *ReceiptStore) validateReportReferencesLocked(
	ctx context.Context,
	rootFD int,
	report LaneReport,
	requireLatest bool,
) error {
	for index, item := range report.Cases {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.ReceiptID == nil {
			continue
		}
		var record ReceiptRecord
		var found bool
		var err error
		if requireLatest {
			key, keyErr := CaseKey(item.Identity)
			if keyErr != nil {
				return keyErr
			}
			record, found, err = store.latestRecordLocked(ctx, rootFD, key)
			if err == nil && found && (record.ID != *item.ReceiptID || record.Availability != item.Availability) {
				found = false
			}
		} else {
			record, found, err = store.findReceiptRecordLocked(ctx, rootFD, item.Identity, *item.ReceiptID, item.Availability)
		}
		if err != nil {
			return fmt.Errorf("report case %d receipt reference: %w", index, err)
		}
		if !found {
			return fmt.Errorf("report case %d does not reference an exact immutable receipt", index)
		}
		if item.Reused && (!proofPassed(record.Receipt.Status) || item.Availability != contract.LaneEligible) {
			return fmt.Errorf("report case %d marks non-passing evidence as reused", index)
		}
	}
	return nil
}

func validateLaneReportStructure(report LaneReport) error {
	if report.SchemaVersion != ReportSchemaVersion {
		return fmt.Errorf("proof lane report schema version is unsupported")
	}
	if !validIdentityPart(report.ConnectorID) {
		return fmt.Errorf("proof lane report connector identity is invalid")
	}
	if err := contract.ValidateLane(report.Lane); err != nil {
		return fmt.Errorf("proof lane report lane is invalid: %w", err)
	}
	if err := report.InventoryHash.Validate(); err != nil {
		return fmt.Errorf("proof lane report inventory hash is invalid: %w", err)
	}
	if report.GeneratedAt.IsZero() || report.GeneratedAt.Location() != time.UTC {
		return fmt.Errorf("proof lane report generation time must be explicit UTC")
	}
	if report.Cases == nil {
		return fmt.Errorf("proof lane report cases must be an explicit array")
	}
	seen := make(map[string]struct{}, len(report.Cases))
	for index, item := range report.Cases {
		if err := validateCaseIdentity(item.Identity); err != nil {
			return fmt.Errorf("report case %d identity: %w", index, err)
		}
		if item.Identity.ConnectorID != report.ConnectorID || item.Identity.Lane != report.Lane {
			return fmt.Errorf("report case %d does not belong to the report connector and lane", index)
		}
		if !validLaneStatus(item.Availability) || !validReason(item.Reason) {
			return fmt.Errorf("report case %d availability or reason is invalid", index)
		}
		key, err := CaseKey(item.Identity)
		if err != nil {
			return fmt.Errorf("report case %d identity: %w", index, err)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("report case %d duplicates an exact case identity", index)
		}
		seen[key] = struct{}{}
		if item.ReceiptID == nil {
			if item.Reused {
				return fmt.Errorf("report case %d claims reuse without a receipt", index)
			}
			continue
		}
		if err := item.ReceiptID.Validate(); err != nil {
			return fmt.Errorf("report case %d receipt ID is invalid: %w", index, err)
		}
	}
	return nil
}

func reportFileName(connectorID string, lane contract.Lane) (string, error) {
	if !validIdentityPart(connectorID) {
		return "", fmt.Errorf("proof report connector identity is invalid")
	}
	if err := contract.ValidateLane(lane); err != nil {
		return "", err
	}
	selector := struct {
		ConnectorID string        `json:"connector_id"`
		Lane        contract.Lane `json:"lane"`
	}{ConnectorID: connectorID, Lane: lane}
	document, err := json.Marshal(selector)
	if err != nil {
		return "", err
	}
	digest, err := contract.CanonicalDigest(document)
	if err != nil {
		return "", err
	}
	value, err := semanticDigestHex(digest)
	if err != nil {
		return "", err
	}
	return "report-" + value + ".json", nil
}

func caseLockFileName(key string) (string, error) {
	keyHex, err := semanticDigestHex(contract.SemanticDigest(key))
	if err != nil {
		return "", err
	}
	return "case-" + keyHex + ".lock", nil
}

func receiptFileName(key string, sequence uint64) (string, error) {
	if sequence == 0 {
		return "", fmt.Errorf("proof receipt sequence must be positive")
	}
	keyHex, err := semanticDigestHex(contract.SemanticDigest(key))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("receipt-%s-%020d.json", keyHex, sequence), nil
}

func parseReceiptFileName(name, prefix string) (uint64, error) {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
		return 0, fmt.Errorf("proof receipt filename is malformed")
	}
	sequenceText := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")
	if len(sequenceText) != 20 {
		return 0, fmt.Errorf("proof receipt filename is malformed")
	}
	for _, character := range sequenceText {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("proof receipt filename sequence is malformed")
		}
	}
	sequence, err := strconv.ParseUint(sequenceText, 10, 64)
	if err != nil || sequence == 0 {
		return 0, fmt.Errorf("proof receipt filename sequence is malformed")
	}
	return sequence, nil
}

func semanticDigestHex(digest contract.SemanticDigest) (string, error) {
	if err := digest.Validate(); err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(digest), "sha256:"), nil
}

func forEachRootEntry(ctx context.Context, rootFD int, visit func(string) error) error {
	directoryFD, err := unix.Openat(rootFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open proof receipt directory iterator: %w", err)
	}
	directory := os.NewFile(uintptr(directoryFD), "proof-receipt-directory")
	if directory == nil {
		_ = unix.Close(directoryFD)
		return fmt.Errorf("open proof receipt directory iterator descriptor")
	}
	defer directory.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(directoryReadBatch)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(entry.Name()); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("iterate proof receipt directory: %w", readErr)
		}
	}
}

func readBoundedRegularFile(rootFD int, name string, maximumBytes int64) ([]byte, error) {
	if !validLeafName(name) {
		return nil, fmt.Errorf("proof store leaf name is invalid")
	}
	fd, err := unix.Openat(rootFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open proof store file descriptor")
	}
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG ||
		before.Nlink != 1 || before.Size <= 0 || before.Size > maximumBytes {
		_ = file.Close()
		return nil, fmt.Errorf("proof store member is not a bounded private regular file")
	}
	document, readErr := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	var after unix.Stat_t
	statErr := unix.Fstat(fd, &after)
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil || int64(len(document)) != before.Size ||
		int64(len(document)) > maximumBytes || after.Mode&unix.S_IFMT != unix.S_IFREG ||
		after.Nlink != 1 || after.Size != before.Size || after.Dev != before.Dev || after.Ino != before.Ino {
		return nil, fmt.Errorf("proof store member changed or failed during bounded read")
	}
	return document, nil
}

func (store *ReceiptStore) publishImmutableLocked(ctx context.Context, rootFD int, name string, document []byte) error {
	temporary, err := store.writeTemporaryLocked(ctx, rootFD, ".receipt-", document)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(rootFD, temporary, 0) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Linkat(rootFD, temporary, rootFD, name, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("immutable proof receipt already exists")
		}
		return fmt.Errorf("publish immutable proof receipt: %w", err)
	}
	if err := unix.Unlinkat(rootFD, temporary, 0); err != nil {
		return fmt.Errorf("remove proof receipt publication temporary: %w", err)
	}
	if err := unix.Fsync(rootFD); err != nil {
		return fmt.Errorf("synchronize immutable proof receipt: %w", err)
	}
	return nil
}

func (store *ReceiptStore) replaceAtomicallyLocked(ctx context.Context, rootFD int, name string, document []byte) error {
	if err := requireExistingRegularOrAbsent(rootFD, name); err != nil {
		return err
	}
	temporary, err := store.writeTemporaryLocked(ctx, rootFD, ".report-", document)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(rootFD, temporary, 0) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := requireExistingRegularOrAbsent(rootFD, name); err != nil {
		return err
	}
	if err := unix.Renameat(rootFD, temporary, rootFD, name); err != nil {
		return fmt.Errorf("atomically replace proof lane report: %w", err)
	}
	if err := unix.Fsync(rootFD); err != nil {
		return fmt.Errorf("synchronize proof lane report replacement: %w", err)
	}
	return nil
}

func (store *ReceiptStore) writeTemporaryLocked(ctx context.Context, rootFD int, prefix string, document []byte) (string, error) {
	if int64(len(document)) > store.maximumDocumentBytes {
		return "", fmt.Errorf("proof store document exceeds its bound")
	}
	for range 32 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		name, err := randomLeafName(prefix)
		if err != nil {
			return "", err
		}
		fd, err := unix.Openat(rootFD, name,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create proof store temporary: %w", err)
		}
		file := os.NewFile(uintptr(fd), name)
		if file == nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(rootFD, name, 0)
			return "", fmt.Errorf("open proof store temporary descriptor")
		}
		writeErr := writeAll(file, document)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			_ = unix.Unlinkat(rootFD, name, 0)
			if writeErr != nil {
				return "", fmt.Errorf("write proof store temporary: %w", writeErr)
			}
			return "", fmt.Errorf("close proof store temporary: %w", closeErr)
		}
		return name, nil
	}
	return "", fmt.Errorf("allocate proof store temporary name")
}

func requireExistingRegularOrAbsent(rootFD int, name string) error {
	if !validLeafName(name) {
		return fmt.Errorf("proof store leaf name is invalid")
	}
	var stat unix.Stat_t
	err := unix.Fstatat(rootFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing proof lane report: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return fmt.Errorf("existing proof lane report is not a private regular file")
	}
	return nil
}

func randomLeafName(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate proof store temporary name: %w", err)
	}
	return prefix + hex.EncodeToString(random[:]) + ".tmp", nil
}

func validLeafName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 &&
		!strings.ContainsAny(name, "/\\\x00")
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) != 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func decodeStoredJSON(document []byte, target any) error {
	if len(document) == 0 {
		return fmt.Errorf("stored JSON document is empty")
	}
	if !utf8.Valid(document) {
		return fmt.Errorf("stored JSON document contains invalid UTF-8")
	}
	if err := validateStoredJSONUnicode(document); err != nil {
		return err
	}
	if err := validateStoredJSONShape(document, reflect.TypeOf(target)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("stored JSON has a trailing value")
		}
		return err
	}
	return nil
}

func validateStoredJSONUnicode(document []byte) error {
	inString := false
	for index := 0; index < len(document); index++ {
		switch document[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || index+1 >= len(document) {
				continue
			}
			if document[index+1] != 'u' {
				index++
				continue
			}
			code, ok := decodeStoredHexQuad(document[index+2:])
			if !ok {
				continue
			}
			switch {
			case code >= 0xd800 && code <= 0xdbff:
				if index+12 > len(document) || document[index+6] != '\\' || document[index+7] != 'u' {
					return fmt.Errorf("stored JSON contains an unpaired UTF-16 surrogate")
				}
				low, validLow := decodeStoredHexQuad(document[index+8:])
				if !validLow || low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("stored JSON contains an unpaired UTF-16 surrogate")
				}
				index += 11
			case code >= 0xdc00 && code <= 0xdfff:
				return fmt.Errorf("stored JSON contains an unpaired UTF-16 surrogate")
			default:
				index += 5
			}
		}
	}
	return nil
}

func decodeStoredHexQuad(document []byte) (uint16, bool) {
	if len(document) < 4 {
		return 0, false
	}
	var value uint16
	for _, character := range document[:4] {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value |= uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value |= uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value |= uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func validateStoredJSONShape(document []byte, target reflect.Type) error {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if err := scanStoredJSONValue(decoder, 0, target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("stored JSON has a trailing value")
		}
		return err
	}
	return nil
}

func scanStoredJSONValue(decoder *json.Decoder, depth int, target reflect.Type) error {
	if depth > maximumStoredJSONDepth {
		return fmt.Errorf("stored JSON exceeds its nesting bound")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	target = storedJSONType(target)
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return fmt.Errorf("stored JSON object member is malformed")
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("stored JSON object member is duplicated")
			}
			seen[name] = struct{}{}
			fieldType, declared := storedJSONObjectFieldType(target, name)
			if !declared {
				return fmt.Errorf("stored JSON object contains an unsupported or wrongly cased field")
			}
			if err := scanStoredJSONValue(decoder, depth+1, fieldType); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("stored JSON object is malformed")
		}
	case '[':
		var elementType reflect.Type
		if target != nil && (target.Kind() == reflect.Array || target.Kind() == reflect.Slice) {
			elementType = target.Elem()
		}
		for decoder.More() {
			if err := scanStoredJSONValue(decoder, depth+1, elementType); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("stored JSON array is malformed")
		}
	default:
		return fmt.Errorf("stored JSON delimiter is malformed")
	}
	return nil
}

func storedJSONType(target reflect.Type) reflect.Type {
	for target != nil && target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	return target
}

func storedJSONObjectFieldType(target reflect.Type, name string) (reflect.Type, bool) {
	target = storedJSONType(target)
	if target == nil || target.Kind() == reflect.Interface {
		return nil, true
	}
	if target.Kind() == reflect.Map {
		return target.Elem(), true
	}
	if target.Kind() != reflect.Struct {
		return nil, true
	}
	for index := range target.NumField() {
		field := target.Field(index)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("json")
		jsonName, _, _ := strings.Cut(tag, ",")
		if jsonName == "-" {
			continue
		}
		if jsonName == "" {
			jsonName = field.Name
		}
		if jsonName == name {
			return field.Type, true
		}
	}
	return nil, false
}
