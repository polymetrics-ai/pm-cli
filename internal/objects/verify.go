package objects

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func (backend *S3) verifyRemote(ctx context.Context, ref contract.ObjectRef, destination io.Writer) error {
	version, etag, err := generationCondition(ref.BackendGeneration)
	if err != nil {
		return err
	}
	response, err := backend.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:    aws.String(backend.bucket),
		Key:       aws.String(ref.ObjectAttemptKey),
		VersionId: version,
		IfMatch:   etag,
	})
	if err != nil {
		return unavailable(ctx)
	}
	if response.Body == nil {
		return ErrObjectIntegrity
	}
	defer response.Body.Close()
	generation, err := backendGeneration(aws.ToString(response.VersionId), aws.ToString(response.ETag))
	if err != nil || generation != ref.BackendGeneration || response.ContentLength == nil ||
		*response.ContentLength != ref.ByteLength || aws.ToString(response.ContentType) != ref.MediaType ||
		response.ContentRange != nil {
		return ErrObjectIntegrity
	}
	hasher := sha256.New()
	observed, err := io.Copy(io.MultiWriter(destination, hasher), io.LimitReader(response.Body, ref.ByteLength+1))
	if err != nil {
		return unavailable(ctx)
	}
	if ctx.Err() != nil {
		return unavailable(ctx)
	}
	var digest [sha256.Size]byte
	hasher.Sum(digest[:0])
	var encoded [sha256.Size * 2]byte
	hex.Encode(encoded[:], digest[:])
	if observed != ref.ByteLength || string(encoded[:]) != ref.SHA256 {
		return ErrObjectIntegrity
	}
	return nil
}

func (backend *S3) newScratchFile() (*os.File, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, ErrObjectUnavailable
	}
	name := "verify-" + hex.EncodeToString(nonce[:])
	file, err := backend.scratch.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, ErrObjectUnavailable
	}
	// Unlink before any provider data is written. The open descriptor remains
	// usable, but process death cannot leave an authoritative or residual file.
	if err := backend.scratch.Remove(name); err != nil {
		_ = file.Close()
		_ = backend.scratch.Remove(name)
		return nil, ErrObjectUnavailable
	}
	return file, nil
}

type verifiedReader struct {
	ctx              context.Context
	file             *os.File
	section          *io.SectionReader
	release          func()
	mutex            sync.Mutex
	closed           bool
	stopCancellation func() bool
}

func newVerifiedReader(ctx context.Context, file *os.File, selection ByteRange, release func()) *verifiedReader {
	reader := &verifiedReader{
		ctx:     ctx,
		file:    file,
		section: io.NewSectionReader(file, selection.Offset, selection.Length),
		release: release,
	}
	reader.mutex.Lock()
	reader.stopCancellation = context.AfterFunc(ctx, func() { _ = reader.Close() })
	reader.mutex.Unlock()
	return reader
}

func (reader *verifiedReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		_ = reader.Close()
		return 0, err
	}
	return reader.section.Read(buffer)
}

func (reader *verifiedReader) Close() error {
	reader.mutex.Lock()
	if reader.closed {
		reader.mutex.Unlock()
		return nil
	}
	reader.closed = true
	stop := reader.stopCancellation
	reader.mutex.Unlock()
	if stop != nil {
		stop()
	}
	err := reader.file.Close()
	reader.release()
	if err != nil {
		return ErrObjectUnavailable
	}
	return nil
}
