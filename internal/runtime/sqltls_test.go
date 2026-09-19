package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// TestStrictSQLTLS asserts V35 against the real fixture PostgreSQL: a wrong CA
// and a wrong hostname both refuse without any plaintext or verify-less
// downgrade, while the intended strict connection works under the isolated
// customer role's operation-scoped permissions.
func TestStrictSQLTLS(t *testing.T) {
	fixture := openSQLFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	now := time.Now().UTC()

	t.Run("intended_strict_connection_succeeds", func(t *testing.T) {
		operation := sqlTestOperation(t, contract.PostgreSQLStatementMetadata, fixture.relation.Binding)
		observation, err := fixture.execute(t, ctx, sqlTestCall(t, operation, fixture.connection(), fixture.relation, nil, sqlTestLimits(), now))
		if err != nil || observation.Outcome != SQLOutcomeKnownCommit {
			t.Fatalf("strict connection observation=%#v error=%v", observation, err)
		}
		witness := observation.TLS
		if err := witness.Validate(); err != nil {
			t.Fatalf("strict TLS witness is incomplete: %v", err)
		}
		if witness.ServerName != sqlFixtureServerName || witness.ProtocolVersion < tls.VersionTLS12 ||
			witness.PeerCertificateSHA256 != fixture.serverCertificateSHA256 {
			t.Fatalf("strict TLS witness does not describe the selected fixture server: %#v", witness)
		}
		// The witness is checked against the server's own view of an
		// independently opened strict session, which also proves the session is
		// genuinely encrypted rather than merely configured to be.
		customer := fixture.customerConnection(t, ctx)
		session, err := ObserveStrictSQLTLS(customer.Conn(), sqlFixtureServerName, time.Now().UTC())
		if err != nil {
			t.Fatalf("witness the independent strict session: %v", err)
		}
		rows, err := sqlQuery(ctx, customer, `SELECT ssl::text AS ssl, version AS version, cipher AS cipher FROM pg_catalog.pg_stat_ssl WHERE pid = pg_backend_pid()`)
		if err != nil || len(rows) != 1 || *rows[0]["ssl"] != "true" {
			t.Fatalf("fixture server did not report an encrypted session: %v rows=%v", err, rows)
		}
		if want := strings.Replace(session.ProtocolName(), " ", "v", 1); *rows[0]["version"] != want {
			t.Fatalf("negotiated protocol %q differs from the server-observed %q", session.ProtocolName(), *rows[0]["version"])
		}
		if session.ProtocolVersion == tls.VersionTLS13 && *rows[0]["cipher"] != session.CipherSuiteName() {
			t.Fatalf("negotiated cipher %q differs from the server-observed %q", session.CipherSuiteName(), *rows[0]["cipher"])
		}
		verifyScopeForStrictTLS(t, ctx, fixture, customer)
	})

	t.Run("wrong_ca_refuses_without_downgrade", func(t *testing.T) {
		connection := fixture.connection()
		connection.TLS = SQLTLSConfig{CACertificatePEM: sqlTestForeignCA(t), ServerName: sqlFixtureServerName}
		observation, err := fixture.executeObserved(t, ctx, connection, &sqlProtocolProbe{})
		if err == nil {
			t.Fatalf("an untrusted certificate was accepted: %#v", observation)
		}
		var unknownAuthority x509.UnknownAuthorityError
		if !errors.As(err, &unknownAuthority) {
			t.Fatalf("wrong CA error = %v; want a certificate verification refusal", err)
		}
	})

	t.Run("wrong_hostname_refuses_without_downgrade", func(t *testing.T) {
		connection := fixture.connection()
		connection.TLS = SQLTLSConfig{CACertificatePEM: fixture.caPEM, ServerName: "wrong-hostname.invalid"}
		observation, err := fixture.executeObserved(t, ctx, connection, &sqlProtocolProbe{})
		if err == nil {
			t.Fatalf("a mismatched hostname was accepted: %#v", observation)
		}
		var hostname x509.HostnameError
		if !errors.As(err, &hostname) {
			t.Fatalf("wrong hostname error = %v; want a certificate verification refusal", err)
		}
	})

	t.Run("weakening_requests_are_refused", func(t *testing.T) {
		for _, testCase := range []struct {
			name   string
			config SQLTLSConfig
		}{
			{name: "skip verification", config: SQLTLSConfig{CACertificatePEM: fixture.caPEM, ServerName: sqlFixtureServerName, InsecureSkipVerify: true}},
			{name: "plaintext downgrade", config: SQLTLSConfig{CACertificatePEM: fixture.caPEM, ServerName: sqlFixtureServerName, AllowPlaintext: true}},
			{name: "protocol floor", config: SQLTLSConfig{CACertificatePEM: fixture.caPEM, ServerName: sqlFixtureServerName, MinimumVersion: tls.VersionTLS11}},
			{name: "absent trust anchor", config: SQLTLSConfig{ServerName: sqlFixtureServerName}},
			{name: "absent server name", config: SQLTLSConfig{CACertificatePEM: fixture.caPEM}},
			{name: "non-certificate trust anchor", config: SQLTLSConfig{CACertificatePEM: []byte("not a certificate"), ServerName: sqlFixtureServerName}},
			{name: "leaf certificate as trust anchor", config: SQLTLSConfig{CACertificatePEM: sqlTestLeafCertificate(t), ServerName: sqlFixtureServerName}},
		} {
			if _, err := NewStrictSQLTLS(testCase.config); err == nil {
				t.Fatalf("%s was accepted as a strict TLS configuration", testCase.name)
			}
		}
		// A weakening request is refused before any physical attempt, so the
		// configuration attempt can never reach the customer endpoint.
		connection := fixture.connection()
		connection.TLS.AllowPlaintext = true
		probe := &sqlProtocolProbe{}
		observation, err := fixture.executeObserved(t, ctx, connection, probe)
		if err == nil || observation.PhysicalSend {
			t.Fatalf("a plaintext request produced a physical send: %#v error=%v", observation, err)
		}
		begins, statements, commits := probe.counters()
		if begins != 0 || statements != 0 || commits != 0 {
			t.Fatalf("a plaintext request reached the server: %d/%d/%d", begins, statements, commits)
		}
	})

	t.Run("plaintext_session_cannot_witness_strict_tls", func(t *testing.T) {
		// The fixture endpoint accepts an unencrypted session, so the strict
		// boundary must be what refuses it rather than the server.
		address := net.JoinHostPort(sqlFixtureServerName, strconv.Itoa(int(fixture.port)))
		plaintext, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			t.Fatalf("dial the fixture endpoint without TLS: %v", err)
		}
		defer plaintext.Close()
		if _, err := ObserveStrictSQLTLS(plaintext, sqlFixtureServerName, now); err == nil {
			t.Fatal("a plaintext connection produced a strict TLS witness")
		}
		if _, err := ObserveStrictSQLTLS(nil, sqlFixtureServerName, now); err == nil {
			t.Fatal("an absent connection produced a strict TLS witness")
		}
	})
}

// executeObserved runs one call while recording the exact protocol sends that
// reached the endpoint, and requires every refusal to stay off the wire.
func (fixture *sqlFixture) executeObserved(t *testing.T, ctx context.Context, connection SQLConnection, probe *sqlProtocolProbe) (SQLObservation, error) {
	t.Helper()
	operation := sqlTestOperation(t, contract.PostgreSQLStatementMetadata, fixture.relation.Binding)
	observation, err := fixture.executeWith(
		t, ctx,
		sqlTestCall(t, operation, connection, fixture.relation, nil, sqlTestLimits(), time.Now().UTC()),
		probe,
	)
	begins, statements, commits := probe.counters()
	if err == nil && (begins == 0 || statements == 0 || commits == 0) {
		t.Fatalf("accepted call did not reach every admitted boundary: %d/%d/%d", begins, statements, commits)
	}
	if err != nil && (observation.PhysicalSend || begins != 0 || statements != 0 || commits != 0) {
		t.Fatalf("refused call reached the endpoint: %#v sends=%d/%d/%d error=%v", observation, begins, statements, commits, err)
	}
	return observation, err
}
