package githubcompat

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	// maximumRequestBody is the largest request body this destination reads. The
	// authored operation is a GET with no body, so a larger body is refused
	// rather than hashed in part.
	maximumRequestBody = 1 << 20

	// maximumRequestHeaderBytes bounds the received request head.
	maximumRequestHeaderBytes = 64 << 10

	// requestHeaderTimeout, requestIdleTimeout and shutdownTimeout bound this
	// destination's own serving work.
	requestHeaderTimeout = 10 * time.Second
	requestIdleTimeout   = 60 * time.Second
	shutdownTimeout      = 5 * time.Second

	// jsonMediaType is the media type of every authored JSON response, and
	// redactedValue replaces the value of a query parameter this destination
	// classifies as secret-bearing.
	jsonMediaType = "application/json; charset=utf-8"
	redactedValue = "redacted"
)

// The authored GitHub-shaped error documents this destination serves. Every
// refusal names the same documentation origin the pinned source declares, so a
// client sees one provider error shape and never a generated one.
const (
	badRequestDocument       = `{"message":"Bad Request","documentation_url":"https://docs.github.com/rest"}`
	notFoundDocument         = `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`
	methodNotAllowedDocument = `{"message":"Method Not Allowed","documentation_url":"https://docs.github.com/rest"}`
	requestTooLargeDocument   = `{"message":"Request Entity Too Large","documentation_url":"https://docs.github.com/rest"}`
	internalErrorDocument    = `{"message":"Internal Server Error","documentation_url":"https://docs.github.com/rest"}`
)

// Config declares one controlled GitHub-compatible destination.
type Config struct {
	// Listen is the declared TCP listen address, host:port. Port 0 binds an
	// ephemeral port and the bound address is reported in Facts.
	Listen string
	// Origin is the admitted provider origin this destination impersonates: its
	// host is the first certificate name and every authored continuation link
	// uses it. It must be an absolute HTTPS origin without credentials, query or
	// fragment, and it must be the exact origin the connection's source admits,
	// because PM refuses every other selected origin.
	Origin string
	// Hosts declares extra certificate names beyond the origin host and the
	// loopback names.
	Hosts []string
	// TLSDir is the absolute directory that holds the generated trust authority
	// and server certificate. The operator declares <TLSDir>/ca.pem as the
	// provider egress trust root and changes no system or global trust store.
	TLSDir string
	// Journal is the absolute path of the observation journal. The proof runner
	// reads exactly this file.
	Journal string
	// Fixture selects the authored fixture behavior.
	Fixture string
	// FaultField selects the required issue member when the selected fixture is
	// one of the two field faults.
	FaultField string
	// Diagnostics receives this destination's own diagnostics. Nil discards them.
	Diagnostics io.Writer
}

// Facts are the resolved operational facts of one prepared destination.
type Facts struct {
	// Listen is the bound listen address.
	Listen string
	// Origin is the canonical admitted origin.
	Origin string
	// CertificateNames are the certificate names the server certificate covers,
	// in order.
	CertificateNames []string
	// CAPath is the generated authority the operator declares as the trust root.
	CAPath string
	// JournalPath is the observation journal this destination appends to.
	JournalPath string
	// Fixture is the canonical fixture value.
	Fixture string
	// FixtureScenario names the authored scenarios that need the selection, empty
	// when the selection is endpoint-defined.
	FixtureScenario string
	// FixtureBehavior documents the response the selection serves.
	FixtureBehavior string
}

// Server is one prepared destination. Prepare validates every declaration and
// generates or reuses the declared TLS material; Run binds, announces, serves and
// records.
type Server struct {
	configuration Config
	selected      authoredFixture
	records       []record
	origin        string
	certificate   tls.Certificate
	names         []string
	journal       *journal
	logger        *log.Logger

	mu      sync.Mutex
	address string
	failure error
	stop    context.CancelFunc
}

// Prepare validates the declared configuration, resolves the selected fixture
// and prepares the declared trust authority and server certificate. It performs
// no network work and never contacts a provider.
func Prepare(configuration Config) (*Server, error) {
	origin, originHost, err := canonicalOrigin(configuration.Origin)
	if err != nil {
		return nil, err
	}
	if configuration.Listen == "" {
		return nil, fmt.Errorf("%w: a listen address is required", ErrConfiguration)
	}
	if _, _, err := net.SplitHostPort(configuration.Listen); err != nil {
		return nil, fmt.Errorf("%w: the listen address must be host:port", ErrConfiguration)
	}
	if configuration.TLSDir == "" || strings.ContainsRune(configuration.TLSDir, '\x00') || !filepath.IsAbs(configuration.TLSDir) {
		return nil, fmt.Errorf("%w: the tls directory must be a declared absolute path", ErrConfiguration)
	}
	selected, err := selectFixture(configuration.Fixture)
	if err != nil {
		return nil, err
	}
	field, err := resolveFaultField(selected, configuration.FaultField)
	if err != nil {
		return nil, err
	}
	records, err := selected.pageRecords(field)
	if err != nil {
		return nil, err
	}
	journalFile, err := openJournal(configuration.Journal)
	if err != nil {
		return nil, err
	}
	names := certificateNames(originHost, configuration.Hosts)
	certificatePEM, keyPEM, err := loadTLSMaterial(filepath.Clean(configuration.TLSDir), names, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load generated server certificate: %w", err)
	}
	var logger *log.Logger
	if configuration.Diagnostics != nil {
		logger = log.New(configuration.Diagnostics, "githubcompat: ", 0)
	}
	return &Server{
		configuration: configuration,
		selected:      selected,
		records:       records,
		origin:        origin,
		certificate:   certificate,
		names:         names,
		journal:       journalFile,
		logger:        logger,
	}, nil
}

// canonicalOrigin returns the canonical origin string and its host name.
func canonicalOrigin(origin string) (string, string, error) {
	if origin == "" {
		return "", "", fmt.Errorf("%w: an admitted origin is required", ErrConfiguration)
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" ||
		!strings.EqualFold(parsed.Scheme, "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("%w: the origin must be an absolute HTTPS origin without credentials, query or fragment", ErrConfiguration)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", "", fmt.Errorf("%w: the origin must not carry a path", ErrConfiguration)
	}
	host := parsed.Hostname()
	if host == "" {
		return "", "", fmt.Errorf("%w: the origin must name a host", ErrConfiguration)
	}
	return "https://" + strings.ToLower(parsed.Host), host, nil
}

// Facts returns the resolved facts of this destination. Run announces them once
// it is bound, and they may be read afterwards.
func (server *Server) Facts() Facts {
	server.mu.Lock()
	defer server.mu.Unlock()
	return Facts{
		Listen:           server.address,
		Origin:           server.origin,
		CertificateNames: append([]string{}, server.names...),
		CAPath:           filepath.Join(filepath.Clean(server.configuration.TLSDir), caCertificateName),
		JournalPath:      server.journal.path,
		Fixture:          server.selected.Value,
		FixtureScenario:  server.selected.Scenario,
		FixtureBehavior:  server.selected.Behavior,
	}
}

// Run binds the declared address, announces the resolved facts once and serves
// until the context is cancelled or the destination fails terminally. Requests
// are served one at a time, so the journal is appended in arrival order.
func (server *Server) Run(ctx context.Context, announce func(Facts)) error {
	if ctx == nil {
		return errors.New("controlled destination requires a context")
	}
	if announce == nil {
		announce = func(Facts) {}
	}
	listener, err := net.Listen("tcp", server.configuration.Listen)
	if err != nil {
		return fmt.Errorf("bind controlled destination: %w", err)
	}
	serving := &http.Server{
		Handler:           http.HandlerFunc(server.handle),
		ReadHeaderTimeout: requestHeaderTimeout,
		IdleTimeout:       requestIdleTimeout,
		MaxHeaderBytes:    maximumRequestHeaderBytes,
	}
	if server.logger != nil {
		serving.ErrorLog = server.logger
	} else {
		serving.ErrorLog = log.New(io.Discard, "", 0)
	}
	serveContext, stop := context.WithCancel(ctx)
	defer stop()
	server.mu.Lock()
	server.address = listener.Addr().String()
	server.stop = stop
	server.mu.Unlock()
	announce(server.Facts())
	watcher := make(chan struct{})
	go func() {
		select {
		case <-serveContext.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			if err := serving.Shutdown(shutdownContext); err != nil {
				_ = serving.Close()
			}
		case <-watcher:
		}
	}()
	serveErr := serving.Serve(tls.NewListener(listener, tlsConfiguration(server.certificate)))
	close(watcher)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve controlled destination: %w", serveErr)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.failure
}

// handle serves one request under the destination's single-flight lock, appends
// exactly one journal entry for it and then writes the planned response, so a
// client that observed a response always finds its entry recorded.
func (server *Server) handle(writer http.ResponseWriter, request *http.Request) {
	server.mu.Lock()
	defer server.mu.Unlock()
	body, oversized, err := readRequestBody(request)
	if err != nil {
		server.failServing(writer, request, fmt.Errorf("read request body: %w", err))
		return
	}
	served := server.plan(request, oversized)
	if err := server.journal.append(server.observationFor(request, body, served)); err != nil {
		server.failServing(writer, request, err)
		return
	}
	writeResponse(writer, served)
}

// plan returns the exact response this destination serves for one request. Every
// deviation from the authored page is explicit: an undeclared route, repository
// or method, an oversized body, a refused query value, a selected fixture fault,
// or the authored page itself.
func (server *Server) plan(request *http.Request, oversized bool) response {
	if oversized {
		return jsonResponse(http.StatusRequestEntityTooLarge, requestTooLargeDocument)
	}
	if request.URL.Path != fixtureRoute {
		return jsonResponse(http.StatusNotFound, notFoundDocument)
	}
	if request.Method != http.MethodGet {
		served := jsonResponse(http.StatusMethodNotAllowed, methodNotAllowedDocument)
		served.headers = append(served.headers, member{name: "allow", value: http.MethodGet})
		return served
	}
	if status := server.selected.forcedStatus(); status != 0 {
		if status == http.StatusUnprocessableEntity {
			return jsonResponse(status, validationFailedDocument("page"))
		}
		return jsonResponse(status, notFoundDocument)
	}
	values, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, badRequestDocument)
	}
	query, refusedField, err := parseRequestQuery(values)
	if err != nil {
		return jsonResponse(http.StatusUnprocessableEntity, validationFailedDocument(refusedField))
	}
	if location := server.selected.redirectLocation(query.page, query.perPage); location != "" {
		return response{
			status:  http.StatusFound,
			headers: []member{{name: "location", value: location}},
		}
	}
	page := authoredWindow(server.records, query.page, query.perPage, server.selected.fault == faultProviderClamp)
	served := jsonResponse(http.StatusOK, encodePage(page.records))
	if link := server.selected.continuationLink(server.origin, query.page, page.limit, page.more); link != "" {
		served.headers = append(served.headers, member{name: "link", value: link})
	}
	return served
}

// failServing answers one request the destination could not record, reports the
// failure and stops serving: a destination that cannot write its own observation
// journal must not keep serving requests the proof runner could never correlate.
func (server *Server) failServing(writer http.ResponseWriter, request *http.Request, failure error) {
	server.failure = failure
	writeResponse(writer, jsonResponse(http.StatusInternalServerError, internalErrorDocument))
	server.logf("%s %s could not be recorded: %v", request.Method, request.URL.Path, failure)
	if server.stop != nil {
		server.stop()
	}
}

// logf reports one destination diagnostic. A diagnostic names the method and the
// path only, never a received field value, so it cannot copy provider material.
func (server *Server) logf(format string, arguments ...any) {
	if server.logger == nil {
		return
	}
	server.logger.Printf(format, arguments...)
}

// response is one fully planned response: the status, this destination's own
// headers in served order, and the exact bytes written to the wire.
type response struct {
	status  int
	headers []member
	body    []byte
}

// jsonResponse returns one authored JSON response: the exact document, its media
// type and its exact length.
func jsonResponse(status int, document string) response {
	return response{
		status: status,
		headers: []member{
			{name: "content-type", value: jsonMediaType},
			{name: "content-length", value: strconv.Itoa(len(document))},
		},
		body: []byte(document),
	}
}

// writeResponse writes one planned response exactly: the declared status, this
// destination's own headers in order, then the exact bytes.
func writeResponse(writer http.ResponseWriter, served response) {
	for _, field := range served.headers {
		writer.Header().Set(field.name, field.value)
	}
	writer.WriteHeader(served.status)
	if len(served.body) == 0 {
		return
	}
	_, _ = writer.Write(served.body)
}

// headerValue returns one served header value, empty when the response does not
// carry that header.
func headerValue(headers []member, name string) string {
	for _, field := range headers {
		if field.name == name {
			return field.value
		}
	}
	return ""
}

// observationFor returns the journal entry of one served request: what this
// destination received and exactly what it returned.
func (server *Server) observationFor(request *http.Request, body []byte, served response) journalEntry {
	fields, hidden, redactions := requestFields(request)
	entry := journalEntry{
		Method:             request.Method,
		Target:             observedTarget(request.RequestURI, hidden),
		MediaType:          requestMediaType(request),
		Status:             served.status,
		ResponseMediaType:  headerValue(served.headers, "content-type"),
		ResponseBodySHA256: hashOf(served.body),
		RequestFields:      fields,
		ResponseFields: map[string]json.RawMessage{
			"headers": rawObject(servedHeaders(served)),
		},
		Redactions: redactions,
	}
	if len(body) != 0 {
		entry.RequestBodySHA256 = hashOf(body)
	}
	return entry
}

// servedHeaders returns the served headers as recorded fields.
func servedHeaders(served response) map[string]json.RawMessage {
	fields := make(map[string]json.RawMessage, len(served.headers))
	for _, field := range served.headers {
		fields[field.name] = rawTextField(field.value)
	}
	return fields
}

// requestFields returns the recorded request fields, the query parameter names
// this destination refuses to record, and the redactions that replace them. The
// recorded fields carry the exact received text of every non-secret query value
// and header value, and nothing else.
func requestFields(request *http.Request) (map[string]json.RawMessage, map[string]struct{}, []contract.ProofRedaction) {
	hidden := make(map[string]struct{})
	redactions := make([]contract.ProofRedaction, 0, 2)
	query := make(map[string]json.RawMessage)
	for _, parameter := range queryParameters(request.URL.RawQuery) {
		if secretBearing(parameter.name) {
			hidden[parameter.name] = struct{}{}
			redactions = append(redactions, excludedRedaction("query", parameter.name))
			continue
		}
		if _, duplicate := query[parameter.name]; duplicate {
			continue
		}
		query[parameter.name] = rawTextField(parameter.value)
	}
	headers := make(map[string]json.RawMessage)
	for name, values := range request.Header {
		lowered := strings.ToLower(name)
		if secretBearing(lowered) {
			redactions = append(redactions, excludedRedaction("headers", lowered))
			continue
		}
		headers[lowered] = rawTextField(strings.Join(values, ", "))
	}
	sort.Slice(redactions, func(left, right int) bool {
		return redactions[left].Pointer < redactions[right].Pointer
	})
	return map[string]json.RawMessage{
		"query":   rawObject(query),
		"headers": rawObject(headers),
	}, hidden, redactions
}

// queryParameter is one query parameter as received, without decoding its value.
type queryParameter struct {
	name  string
	value string
}

// queryParameters splits one received query string into its parameters,
// preserving received order and text. A parameter without a value is reported
// with an empty value and an undecodable name is reported verbatim, so it is
// classified conservatively.
func queryParameters(rawQuery string) []queryParameter {
	if rawQuery == "" {
		return nil
	}
	parts := strings.Split(rawQuery, "&")
	parameters := make([]queryParameter, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		decoded, err := url.QueryUnescape(name)
		if err != nil {
			decoded = name
		}
		parameters = append(parameters, queryParameter{name: decoded, value: value})
	}
	return parameters
}

// observedTarget returns the received request target with the value of every
// secret-bearing query parameter replaced. The target is otherwise exactly what
// arrived, and the replaced field is reported as an excluded redaction instead of
// a value, so the journal never carries provider authentication material.
func observedTarget(requestURI string, hidden map[string]struct{}) string {
	if len(hidden) == 0 {
		return requestURI
	}
	target, query, found := strings.Cut(requestURI, "?")
	if !found {
		return requestURI
	}
	parts := strings.Split(query, "&")
	for index, part := range parts {
		name, _, hasValue := strings.Cut(part, "=")
		if !hasValue {
			continue
		}
		decoded, err := url.QueryUnescape(name)
		if err != nil {
			decoded = name
		}
		if _, secret := hidden[decoded]; secret {
			parts[index] = name + "=" + redactedValue
		}
	}
	return target + "?" + strings.Join(parts, "&")
}

// requestMediaType returns the media type the request declared: its Content-Type
// when one is present, otherwise its first Accept preference, and empty when it
// declared neither.
func requestMediaType(request *http.Request) string {
	if contentType := strings.TrimSpace(request.Header.Get("Content-Type")); contentType != "" {
		return contentType
	}
	accept := request.Header.Get("Accept")
	if accept == "" {
		return ""
	}
	preference := strings.TrimSpace(strings.Split(accept, ",")[0])
	if mediaType, _, err := mime.ParseMediaType(preference); err == nil {
		return mediaType
	}
	return preference
}

// readRequestBody reads one request body in full and reports whether the body
// exceeded the declared bound. An oversized body is refused instead of recorded
// in part.
func readRequestBody(request *http.Request) ([]byte, bool, error) {
	if request.Body == nil {
		return nil, false, nil
	}
	document, err := io.ReadAll(io.LimitReader(request.Body, maximumRequestBody+1))
	if err != nil {
		return nil, false, err
	}
	if len(document) > maximumRequestBody {
		return nil, true, nil
	}
	return document, false, nil
}

// rawObject encodes one recorded field object with its names in sorted order and
// its received text escaped only as JSON requires, so the same received fields
// always produce the same record.
func rawObject(fields map[string]json.RawMessage) json.RawMessage {
	if len(fields) == 0 {
		return json.RawMessage(`{}`)
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	var document bytes.Buffer
	document.WriteByte('{')
	for index, name := range names {
		if index > 0 {
			document.WriteByte(',')
		}
		document.WriteString(quoted(name))
		document.WriteByte(':')
		document.Write(fields[name])
	}
	document.WriteByte('}')
	return json.RawMessage(document.Bytes())
}
