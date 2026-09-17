package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestControlTransactionCommitOutcome(t *testing.T) {
	credentials, observerPool := prepareTestStore(t, 4)

	t.Run("lost_commit_reply_preserves_unknown_and_original_identity", func(t *testing.T) {
		scope := newTestScope(t)
		t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
		request := testAdmissionRequest(t, scope, "commit-reply-lost", "commit-reply-lost")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := WithinScope(ctx, observerPool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := LookupAdmission(ctx, tx, request.Key)
			if !errors.Is(err, ErrNotFound) {
				return errors.New("commit fault fixture identity was not initially absent")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		fault := &commitReplyFault{}
		fault.observeCommitted = func() error {
			observerContext, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				err := WithinScope(observerContext, observerPool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
					accepted, err := LookupAdmission(ctx, tx, request.Key)
					if err != nil {
						return err
					}
					if accepted.RunID != request.Spec.RunID || accepted.IntentDigest != request.Spec.IdempotencyFingerprint {
						return errors.New("independent commit observer found another admission identity")
					}
					return nil
				})
				if !errors.Is(err, ErrNotFound) {
					return err
				}
				select {
				case <-observerContext.Done():
					return observerContext.Err()
				case <-ticker.C:
				}
			}
		}

		// Keep the real role, TLS verification, connection preparation and pool
		// limits. Only withhold one plaintext protocol reply from the driver;
		// neither Commit nor any store result is replaced by a mock.
		configuration := observerPool.inner.Config()
		configuration.MaxConns = 1
		configuration.MinConns = 0
		configuration.MinIdleConns = 0
		buildFrontend := configuration.ConnConfig.BuildFrontend
		if buildFrontend == nil {
			t.Fatal("selected PostgreSQL driver has no protocol frontend builder")
		}
		configuration.ConnConfig.BuildFrontend = func(reader io.Reader, writer io.Writer) *pgproto3.Frontend {
			return buildFrontend(
				&commitReplyFaultReader{reader: reader, fault: fault},
				&commitReplyFaultWriter{writer: writer, fault: fault},
			)
		}
		inner, err := pgxpool.NewWithConfig(ctx, configuration)
		if err != nil {
			t.Fatalf("open bounded commit fault pool: %T", err)
		}
		faultPool := &Pool{inner: inner}
		t.Cleanup(faultPool.Close)

		err = WithinScope(ctx, faultPool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			if _, err := AdmitRun(ctx, tx, request); err != nil {
				return err
			}
			fault.armed.Store(true)
			return nil
		})
		if !fault.commitSent.Load() || !fault.replyDropped.Load() || !fault.commitObserved.Load() {
			t.Fatalf("fault did not withhold a physically committed admission reply: %T", err)
		}
		if !errors.Is(err, ErrCommitOutcomeUnknown) {
			t.Fatalf("committed admission reply loss lacks unknown-outcome classification: %v", err)
		}

		if err := WithinScope(ctx, observerPool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			replayed, err := AdmitRun(ctx, tx, request)
			if err != nil {
				return err
			}
			if !replayed.Replayed || replayed.RunID != request.Spec.RunID || replayed.IntentDigest != request.Spec.IdempotencyFingerprint {
				return errors.New("same-key recovery did not retain the original committed admission")
			}
			var commands int
			if err := tx.queryRow(ctx, `SELECT count(*) FROM pm.outbox_commands WHERE run_id = $1`, request.Spec.RunID).Scan(&commands); err != nil {
				return err
			}
			if commands != 1 {
				return errors.New("same-key recovery duplicated the durable outbox effect")
			}
			return nil
		}); err != nil {
			t.Fatalf("reconcile original durable admission: %T", err)
		}
	})

	t.Run("server_confirmed_commit_rollback_is_not_unknown", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := WithinTransaction(ctx, observerPool, func(ctx context.Context, tx *Transaction) error {
			_, queryErr := tx.exec(ctx, `SELECT 1 / 0`)
			var serverError *pgconn.PgError
			if !errors.As(queryErr, &serverError) || serverError.Code != "22012" {
				return errors.New("fixture did not reach a real PostgreSQL aborted transaction")
			}
			// Deliberately reach COMMIT in the aborted transaction. PostgreSQL
			// returns a ROLLBACK command tag, not an ambiguous lost reply.
			return nil
		})
		if !errors.Is(err, pgx.ErrTxCommitRollback) {
			t.Fatalf("server did not confirm rollback at COMMIT: %T", err)
		}
		if errors.Is(err, ErrCommitOutcomeUnknown) {
			t.Fatal("server-confirmed rollback was classified as unknown")
		}
	})

	t.Run("complete_server_constraint_refusal_is_not_unknown", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		migrator, err := pgx.Connect(ctx, credentials.migratorDSN)
		if err != nil {
			t.Fatalf("open owned commit-refusal fixture setup: %T", err)
		}
		t.Cleanup(func() {
			cleanupContext, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if err := migrator.Close(cleanupContext); err != nil {
				t.Errorf("close owned commit-refusal fixture setup: %T", err)
			}
		})
		table := pgx.Identifier{"pm", "commit_refusal_" + randomHex(t, 8)}.Sanitize()
		if _, err := migrator.Exec(ctx, `CREATE TABLE `+table+` (
			value bigint UNIQUE DEFERRABLE INITIALLY DEFERRED
		)`); err != nil {
			t.Fatalf("create uniquely owned deferred-constraint fixture: %T", err)
		}
		t.Cleanup(func() {
			cleanupContext, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if _, err := migrator.Exec(cleanupContext, `DROP TABLE `+table); err != nil {
				t.Errorf("remove uniquely owned deferred-constraint fixture: %T", err)
			}
		})
		if _, err := migrator.Exec(ctx, `GRANT SELECT, INSERT ON TABLE `+table+` TO pm_app_runtime`); err != nil {
			t.Fatalf("grant runtime access to the owned constraint fixture: %T", err)
		}
		err = WithinTransaction(ctx, observerPool, func(ctx context.Context, tx *Transaction) error {
			_, err := tx.exec(ctx, `INSERT INTO `+table+` (value) VALUES (1), (1)`)
			return err
		})
		var serverError *pgconn.PgError
		if !errors.As(err, &serverError) || serverError.Code != "23505" {
			t.Fatalf("COMMIT did not receive the real deferred-constraint refusal: %T", err)
		}
		if errors.Is(err, ErrCommitOutcomeUnknown) {
			t.Fatal("complete server constraint refusal was classified as unknown")
		}
		if err := WithinTransaction(ctx, observerPool, func(ctx context.Context, tx *Transaction) error {
			var rows int
			if err := tx.queryRow(ctx, `SELECT count(*) FROM `+table).Scan(&rows); err != nil {
				return err
			}
			if rows != 0 {
				return errors.New("server-refused COMMIT retained fixture rows")
			}
			return nil
		}); err != nil {
			t.Fatalf("independently verify refused COMMIT rollback: %T", err)
		}
	})
}

type commitReplyFault struct {
	armed            atomic.Bool
	commitSent       atomic.Bool
	replyDropped     atomic.Bool
	commitObserved   atomic.Bool
	observeCommitted func() error
}

// CommitReplyFaultForTest exposes the physical lost-COMMIT-reply boundary to
// the external public-route integration test without adding a production hook.
type CommitReplyFaultForTest struct {
	fault *commitReplyFault
}

func NewCommitReplyFaultPoolForTest(
	ctx context.Context,
	source *Pool,
) (*Pool, *CommitReplyFaultForTest, error) {
	if ctx == nil || source == nil || source.inner == nil {
		return nil, nil, errors.New("commit-reply fault requires a live source pool")
	}
	fault := &commitReplyFault{}
	configuration := source.inner.Config()
	configuration.MaxConns = 1
	configuration.MinConns = 0
	configuration.MinIdleConns = 0
	buildFrontend := configuration.ConnConfig.BuildFrontend
	if buildFrontend == nil {
		return nil, nil, errors.New("selected PostgreSQL driver has no protocol frontend builder")
	}
	configuration.ConnConfig.BuildFrontend = func(reader io.Reader, writer io.Writer) *pgproto3.Frontend {
		return buildFrontend(
			&commitReplyFaultReader{reader: reader, fault: fault},
			&commitReplyFaultWriter{writer: writer, fault: fault},
		)
	}
	inner, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		return nil, nil, fmt.Errorf("open commit-reply fault pool: %w", err)
	}
	pool := &Pool{inner: inner}
	if err := pool.verifyRuntimeIdentity(ctx); err != nil {
		inner.Close()
		return nil, nil, err
	}
	return pool, &CommitReplyFaultForTest{fault: fault}, nil
}

func (fault *CommitReplyFaultForTest) Arm(observeCommitted func() error) error {
	if fault == nil || fault.fault == nil || observeCommitted == nil {
		return errors.New("commit-reply fault observation is required")
	}
	if fault.fault.armed.Load() {
		return errors.New("commit-reply fault is already armed")
	}
	fault.fault.observeCommitted = observeCommitted
	if !fault.fault.armed.CompareAndSwap(false, true) {
		return errors.New("commit-reply fault is already armed")
	}
	return nil
}

func (fault *CommitReplyFaultForTest) Completed() bool {
	return fault != nil && fault.fault != nil &&
		fault.fault.commitSent.Load() &&
		fault.fault.replyDropped.Load() &&
		fault.fault.commitObserved.Load()
}

type commitReplyFaultReader struct {
	reader io.Reader
	fault  *commitReplyFault
}

func (reader *commitReplyFaultReader) Read(destination []byte) (int, error) {
	n, err := reader.reader.Read(destination)
	if reader.fault.commitSent.Load() && reader.fault.replyDropped.CompareAndSwap(false, true) {
		if observeErr := reader.fault.observeCommitted(); observeErr != nil {
			return 0, observeErr
		}
		reader.fault.commitObserved.Store(true)
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

type commitReplyFaultWriter struct {
	writer io.Writer
	fault  *commitReplyFault
	frame  [4096]byte
	used   int
}

func (writer *commitReplyFaultWriter) Write(data []byte) (int, error) {
	n, err := writer.writer.Write(data)
	if !writer.fault.armed.Load() || writer.fault.replyDropped.Load() || n == 0 {
		return n, err
	}
	if n > len(writer.frame)-writer.used {
		return n, errors.New("commit fault protocol frame exceeds its fixture bound")
	}
	copy(writer.frame[writer.used:], data[:n])
	writer.used += n
	for writer.used >= 5 {
		length := uint64(binary.BigEndian.Uint32(writer.frame[1:5])) + 1
		if length < 5 || length > uint64(len(writer.frame)) {
			return n, errors.New("commit fault protocol frame is invalid")
		}
		if length > uint64(writer.used) {
			break
		}
		size := int(length)
		if writer.frame[0] == 'Q' && writer.frame[size-1] == 0 &&
			bytes.EqualFold(bytes.TrimSpace(writer.frame[5:size-1]), []byte("commit")) {
			writer.fault.commitSent.Store(true)
		}
		copy(writer.frame[:], writer.frame[size:writer.used])
		writer.used -= size
	}
	return n, err
}
