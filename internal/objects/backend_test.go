package objects

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// This controlled S3 peer exercises the real pinned SDK and byte-verification
// path. It is not selected-stack, PostgreSQL publication, or live-cloud proof.
type controlledObject struct {
	body      []byte
	mediaType string
	etag      string
}

type objectPeer struct {
	mutex                  sync.Mutex
	objects                map[string]controlledObject
	puts                   int
	gets                   int
	losePutAcknowledgement bool
	corruptReadback        bool
	backend                *S3
	scratch                string
}

func newObjectPeer(t *testing.T) *objectPeer {
	t.Helper()
	peer := &objectPeer{objects: make(map[string]controlledObject), scratch: t.TempDir()}
	if err := os.Chmod(peer.scratch, 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(peer.serveHTTP))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	var err error
	peer.backend, err = NewS3(S3Configuration{
		Endpoint:         server.URL,
		Bucket:           "owned-artifacts",
		Region:           "us-east-1",
		RootCAs:          roots,
		ScratchDirectory: peer.scratch,
		Bounds:           Bounds{MaxObjectBytes: 1024, MaxInFlight: 1, RequestTimeout: 5 * time.Second},
		LoadCredentials: func(context.Context) (Credentials, error) {
			return Credentials{AccessKeyID: "controlled-access", SecretAccessKey: "controlled-secret"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := peer.backend.Close(); err != nil {
			t.Error(err)
		}
	})
	return peer
}

func (peer *objectPeer) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(request.Body, 1025))
		if err != nil || len(body) > 1024 {
			http.Error(writer, "bounded controlled request refused", http.StatusRequestEntityTooLarge)
			return
		}
		peer.mutex.Lock()
		peer.puts++
		if _, exists := peer.objects[request.URL.Path]; exists {
			peer.mutex.Unlock()
			writeObjectPeerError(writer, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		if request.Header.Get("If-None-Match") != "*" {
			peer.mutex.Unlock()
			writeObjectPeerError(writer, http.StatusBadRequest, "InvalidRequest")
			return
		}
		object := controlledObject{body: body, mediaType: request.Header.Get("Content-Type"), etag: `"controlled-generation"`}
		peer.objects[request.URL.Path] = object
		loseAcknowledgement := peer.losePutAcknowledgement
		peer.mutex.Unlock()
		if loseAcknowledgement {
			writeObjectPeerError(writer, http.StatusInternalServerError, "InternalError")
			return
		}
		writer.Header().Set("ETag", object.etag)
		writer.WriteHeader(http.StatusOK)
	case http.MethodGet:
		peer.mutex.Lock()
		peer.gets++
		object, exists := peer.objects[request.URL.Path]
		object.body = bytes.Clone(object.body)
		corrupt := peer.corruptReadback
		peer.mutex.Unlock()
		if !exists {
			writeObjectPeerError(writer, http.StatusNotFound, "NoSuchKey")
			return
		}
		if request.Header.Get("If-Match") != object.etag {
			writeObjectPeerError(writer, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		if corrupt && len(object.body) != 0 {
			object.body[len(object.body)-1] ^= 1
		}
		writer.Header().Set("ETag", object.etag)
		writer.Header().Set("Content-Type", object.mediaType)
		writer.Header().Set("Content-Length", strconv.Itoa(len(object.body)))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(object.body)
	default:
		writeObjectPeerError(writer, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func writeObjectPeerError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/xml")
	writer.WriteHeader(status)
	_, _ = fmt.Fprintf(writer, "<Error><Code>%s</Code></Error>", code)
}

func objectAttempt(t *testing.T, body []byte) Attempt {
	t.Helper()
	attempt, err := NewAttempt(contract.Scope{OrgID: "org/a", WorkspaceID: "workspace"}, "page-result", "application/octet-stream", body, 1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = attempt.Close() })
	return attempt
}

func uploadObject(t *testing.T, peer *objectPeer, body []byte) contract.ObjectRef {
	t.Helper()
	object, err := peer.backend.Upload(context.Background(), objectAttempt(t, body))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := object.Reference()
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestImmutableObjectAttempts(t *testing.T) {
	peer := newObjectPeer(t)
	original := []byte("exact\x00object\xffbytes")
	attempt := objectAttempt(t, bytes.Clone(original))
	copyOfAttempt := attempt
	start := make(chan struct{})
	type result struct {
		object VerifiedObject
		err    error
	}
	results := make(chan result, 2)
	for _, candidate := range []Attempt{attempt, copyOfAttempt} {
		go func() {
			<-start
			object, err := peer.backend.Upload(context.Background(), candidate)
			results <- result{object: object, err: err}
		}()
	}
	close(start)
	var ref contract.ObjectRef
	succeeded, refused := 0, 0
	for range 2 {
		observed := <-results
		switch {
		case observed.err == nil:
			succeeded++
			var err error
			ref, err = observed.object.Reference()
			if err != nil {
				t.Fatal(err)
			}
		case errors.Is(observed.err, ErrAttemptUsed):
			refused++
		default:
			t.Fatalf("unexpected upload outcome: %v", observed.err)
		}
	}
	peer.mutex.Lock()
	puts := peer.puts
	peer.mutex.Unlock()
	if succeeded != 1 || refused != 1 || puts != 1 {
		t.Fatalf("one immutable attempt: succeeded=%d refused=%d physical puts=%d", succeeded, refused, puts)
	}
	reader, err := peer.backend.OpenVerified(context.Background(), ref, ByteRange{Offset: 3, Length: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	entries, err := os.ReadDir(peer.scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("readable artifact left filesystem residue: entries=%d err=%v", len(entries), err)
	}
	selected, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(selected, original[3:11]) {
		t.Fatalf("range bytes=%x err=%v", selected, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	peer.mutex.Lock()
	gets := peer.gets
	peer.mutex.Unlock()
	foreign := ref
	foreign.OrgID = "org%2Fa"
	if _, err := peer.backend.OpenVerified(context.Background(), foreign, ByteRange{Length: foreign.ByteLength}); !errors.Is(err, ErrObjectIntegrity) {
		t.Fatalf("foreign scope/key binding returned %v", err)
	}
	peer.mutex.Lock()
	defer peer.mutex.Unlock()
	if peer.gets != gets {
		t.Fatal("foreign scope/key binding reached the object backend")
	}
}

func TestCorruptObjectNeverBecomesReadable(t *testing.T) {
	t.Run("upload readback differs despite matching length and generation", func(t *testing.T) {
		peer := newObjectPeer(t)
		peer.mutex.Lock()
		peer.corruptReadback = true
		peer.mutex.Unlock()
		object, err := peer.backend.Upload(context.Background(), objectAttempt(t, []byte("abcdef")))
		if !errors.Is(err, ErrObjectIntegrity) {
			t.Fatalf("corrupt upload was not refused: %v", err)
		}
		if _, err := object.Reference(); !errors.Is(err, ErrObjectIntegrity) {
			t.Fatal("unverified upload exposed a usable object reference")
		}
	})
	t.Run("corruption outside the requested range still blocks every byte", func(t *testing.T) {
		peer := newObjectPeer(t)
		ref := uploadObject(t, peer, []byte("abcdef"))
		peer.mutex.Lock()
		peer.corruptReadback = true
		peer.mutex.Unlock()
		reader, err := peer.backend.OpenVerified(context.Background(), ref, ByteRange{Length: 2})
		if reader != nil || !errors.Is(err, ErrObjectIntegrity) {
			if reader != nil {
				_ = reader.Close()
			}
			t.Fatalf("corrupt registered range became readable: %v", err)
		}
	})
}

func TestUnknownObjectUploadRequiresNewAttempt(t *testing.T) {
	peer := newObjectPeer(t)
	peer.mutex.Lock()
	peer.losePutAcknowledgement = true
	peer.mutex.Unlock()
	first := objectAttempt(t, []byte("durable-but-unacknowledged"))
	firstDescriptor, err := first.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.backend.Upload(context.Background(), first); !errors.Is(err, ErrObjectUnavailable) {
		t.Fatalf("lost PUT acknowledgement returned %v", err)
	}
	if _, err := peer.backend.Upload(context.Background(), first); !errors.Is(err, ErrAttemptUsed) {
		t.Fatalf("uncertain attempt was reusable: %v", err)
	}
	peer.mutex.Lock()
	peer.losePutAcknowledgement = false
	putsAfterUnknown := peer.puts
	peer.mutex.Unlock()
	if putsAfterUnknown != 1 {
		t.Fatalf("SDK or caller repeated an uncertain PUT: %d", putsAfterUnknown)
	}
	second := objectAttempt(t, []byte("durable-but-unacknowledged"))
	secondDescriptor, err := second.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	if firstDescriptor.ObjectAttemptKey == secondDescriptor.ObjectAttemptKey {
		t.Fatal("new physical attempt reused the uncertain object key")
	}
	if _, err := peer.backend.Upload(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	peer.mutex.Lock()
	defer peer.mutex.Unlock()
	if peer.puts != 2 || len(peer.objects) != 2 ||
		!bytes.Equal(peer.objects["/owned-artifacts/"+firstDescriptor.ObjectAttemptKey].body, []byte("durable-but-unacknowledged")) {
		t.Fatal("retry overwrote or removed the uncertain prior object")
	}
}

func TestObjectReadCancellationReleasesCapacity(t *testing.T) {
	peer := newObjectPeer(t)
	ref := uploadObject(t, peer, []byte("retained-object"))
	ctx, cancel := context.WithCancel(context.Background())
	reader, err := peer.backend.OpenVerified(ctx, ref, ByteRange{Length: ref.ByteLength})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer reader.Close()
	cancel()
	checkContext, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := peer.backend.Verify(checkContext, ref); err != nil {
		t.Fatalf("cancelled read retained the only backend capacity slot: %v", err)
	}
	var one [1]byte
	if _, err := reader.Read(one[:]); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled content remained readable: %v", err)
	}
}
