// Command pm-github-fixture runs the controlled GitHub-compatible HTTPS
// destination of the credential-free direct-read demo: it serves the authored
// acme/widgets fixture pages and records one independent provider observation
// entry per served request. It is fake evidence, it never contacts GitHub or
// holds a provider credential, and it changes no system trust store.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/polymetrics-ai/pm-cli/internal/testfixture/githubcompat"
)

const (
	usageFailureExit = 2
	failedExit       = 1
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "pm-github-fixture: %v\n", err)
		if errors.Is(err, errUsage) {
			os.Exit(usageFailureExit)
		}
		os.Exit(failedExit)
	}
}

var errUsage = errors.New("invalid command usage")

type options struct {
	listen     string
	origin     string
	hosts      repeatedFlag
	tlsDir     string
	journal    string
	fixture    string
	faultField string
}

// repeatedFlag collects one repeatable string flag in the order given.
type repeatedFlag []string

func (values *repeatedFlag) String() string {
	return strings.Join(*values, ", ")
}

func (values *repeatedFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	if ctx == nil || stdout == nil || stderr == nil {
		return fmt.Errorf("%w: the controlled destination requires a context and output", errUsage)
	}
	var parsed options
	flags := flag.NewFlagSet("pm-github-fixture", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&parsed.listen, "listen", "127.0.0.1:8443", "declared TCP listen address of the controlled destination")
	flags.StringVar(&parsed.origin, "origin", "https://api.github.com", "admitted provider origin this destination impersonates")
	flags.Var(&parsed.hosts, "host", "extra certificate name to cover (repeatable)")
	flags.StringVar(&parsed.tlsDir, "tls-dir", "", "absolute directory that holds the generated ca.pem, server.pem and their keys")
	flags.StringVar(&parsed.journal, "journal", "", "absolute path of the observation journal this destination appends to")
	flags.StringVar(&parsed.fixture, "fixture", githubcompat.DefaultFixture, "authored fixture selection to serve")
	flags.StringVar(&parsed.faultField, "fault-field", "", "required issue member the two field faults select")
	flags.Usage = func() {
		output := flags.Output()
		_, _ = fmt.Fprint(output, usageText)
		flags.PrintDefaults()
		_, _ = fmt.Fprint(output, fixtureUsage())
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errUsage
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: unexpected positional arguments", errUsage)
	}
	destination, err := githubcompat.Prepare(githubcompat.Config{
		Listen:      parsed.listen,
		Origin:      parsed.origin,
		Hosts:       parsed.hosts,
		TLSDir:      parsed.tlsDir,
		Journal:     parsed.journal,
		Fixture:     parsed.fixture,
		FaultField:  parsed.faultField,
		Diagnostics: stderr,
	})
	if err != nil {
		if errors.Is(err, githubcompat.ErrConfiguration) {
			flags.Usage()
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		return err
	}
	return destination.Run(ctx, func(facts githubcompat.Facts) {
		printFacts(stdout, facts)
	})
}

// printFacts reports the resolved operational facts of one served destination.
// It prints no material from a received request and no secret.
func printFacts(stdout io.Writer, facts githubcompat.Facts) {
	_, _ = fmt.Fprintf(stdout, "githubcompat: listening on https://%s\n", facts.Listen)
	_, _ = fmt.Fprintf(stdout, "  declared origin      %s\n", facts.Origin)
	_, _ = fmt.Fprintf(stdout, "  certificate names    %s\n", strings.Join(facts.CertificateNames, ", "))
	_, _ = fmt.Fprintf(stdout, "  declared trust ca    %s\n", facts.CAPath)
	_, _ = fmt.Fprintf(stdout, "  observation journal  %s\n", facts.JournalPath)
	_, _ = fmt.Fprintf(stdout, "  fixture              %s\n", facts.Fixture)
	if facts.FixtureScenario != "" {
		_, _ = fmt.Fprintf(stdout, "  authored scenarios   %s\n", facts.FixtureScenario)
	}
	_, _ = fmt.Fprintf(stdout, "  served behavior      %s\n", facts.FixtureBehavior)
	_, _ = fmt.Fprintln(stdout, "  note                 the first served request creates the journal; the proof runner refuses a zero-byte journal")
}

// fixtureUsage documents every authored fixture selection from the fixture
// table itself, so the usage text and the served behavior cannot drift apart.
func fixtureUsage() string {
	var builder strings.Builder
	builder.WriteString("\nFixtures (-fixture accepts the canonical value or its trailing name):\n")
	for _, fixture := range githubcompat.Fixtures() {
		builder.WriteString("  " + fixture.Value)
		if fixture.Value == githubcompat.DefaultFixture {
			builder.WriteString(" (default)")
		}
		builder.WriteString("\n")
		if fixture.Scenario != "" {
			builder.WriteString("      authored scenarios: " + fixture.Scenario + "\n")
		} else {
			builder.WriteString("      authored scenarios: none; this selection is endpoint-defined\n")
		}
		builder.WriteString("      " + fixture.Behavior + "\n")
	}
	return builder.String()
}

// usageText documents the operator procedure, the served routes and the
// documented refusal behavior of the controlled destination.
const usageText = `pm-github-fixture serves the authored GitHub-compatible fixture pages of the
credential-free direct-read demo over HTTPS and records one independent provider
observation entry per served request. Every response is authored fake evidence:
this command never contacts GitHub, never holds a provider credential, and a
passing case here never certifies live provider behavior.

Operator procedure:
  1. Declare an absolute -tls-dir and an absolute -journal.
  2. Route the admitted provider origin host to -listen. PM admits only the exact
     origin its connector source declares (default https://api.github.com), so the
     generated server certificate covers that host name first, and no other root
     can complete the handshake. PM's provider transport refuses loopback, private
     and documentation destinations, so the routing must present a permitted
     address for that host; this destination never weakens that policy.
  3. Declare <tls-dir>/ca.pem as the provider egress trust root: install those
     bytes as the ca.pem member of the secret that
     provider_http.tls_trust_root_ref names for the org, workspace and
     connection. Only that declared root is used: this command reads no system or
     global trust store, changes none, and never disables peer verification.
  4. Point PM_PROOF_PROVIDER_JOURNAL at the -journal path, then serve one warm-up
     request before the proof runner starts: the runner refuses a zero-byte
     journal, so a destination that has served nothing has no journal yet.
  5. Select the fixture the authored case needs, serve the case, then read the
     journal entry that request produced. One request is served at a time, so
     journal order is arrival order and the runner's single-flight correlation
     sees exactly one new entry per case step.

Routes and refusals:
  GET /repos/acme/widgets/issues?per_page=<1-100>&page=<1-1000000>
      the authored acme/widgets issue list. per_page defaults to 30 and page to
      1. A missing Link header means the authored source ended.
  Any other route, repository or method, a request body above 1 MiB, and a
  refused query value are answered with an authored GitHub-shaped error body
  (404, 405, 413, 422 or 400) and are recorded in the journal like every other
  served request. Every served request is recorded exactly once, including a
  refusal, and a request that carries provider authentication material is
  recorded as an excluded redaction instead of a value.

`
