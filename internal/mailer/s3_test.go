package mailer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 serves one object, or a failure.
type fakeS3 struct {
	data   []byte
	err    error
	inputs []*s3.GetObjectInput
}

func (f *fakeS3) GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.inputs = append(f.inputs, params)
	if f.err != nil {
		return nil, f.err
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(f.data))}, nil
}

// Given a message an SES receipt rule stored, when it is fetched, then the
// bytes come back from the bucket and key that were announced.
func TestS3ObjectsGetObject(t *testing.T) {
	api := &fakeS3{data: []byte(inboundRaw)}
	objects := NewS3Objects(api, 0)

	got, err := objects.GetObject(context.Background(), "webmail-inbound", "mail/abc123")
	if err != nil {
		t.Fatalf("GetObject() error = %v", err)
	}
	if string(got) != inboundRaw {
		t.Errorf("GetObject() returned %d bytes, want the stored message", len(got))
	}
	if aws.ToString(api.inputs[0].Bucket) != "webmail-inbound" || aws.ToString(api.inputs[0].Key) != "mail/abc123" {
		t.Errorf("GetObject() asked for %+v, want the announced object", api.inputs[0])
	}
}

// Given a stored object larger than the limit, when it is fetched, then it is
// refused rather than buffered: what is in the bucket came from a stranger.
func TestS3ObjectsRefusesOversizedObject(t *testing.T) {
	api := &fakeS3{data: bytes.Repeat([]byte{'x'}, 100)}
	objects := NewS3Objects(api, 50)

	_, err := objects.GetObject(context.Background(), "b", "k")
	if err == nil || !strings.Contains(err.Error(), "over the 50 byte limit") {
		t.Fatalf("GetObject() error = %v, want the size limit", err)
	}
}

// Given an object exactly at the limit, when it is fetched, then it is
// accepted: the limit is inclusive.
func TestS3ObjectsAcceptsObjectAtLimit(t *testing.T) {
	api := &fakeS3{data: bytes.Repeat([]byte{'x'}, 50)}

	got, err := NewS3Objects(api, 50).GetObject(context.Background(), "b", "k")
	if err != nil {
		t.Fatalf("GetObject() error = %v", err)
	}
	if len(got) != 50 {
		t.Fatalf("GetObject() returned %d bytes, want 50", len(got))
	}
}

// Given S3 refusing the request, when an object is fetched, then the failure
// is wrapped with what was being fetched.
func TestS3ObjectsReportsFailure(t *testing.T) {
	wantErr := errors.New("access denied")
	_, err := NewS3Objects(&fakeS3{err: wantErr}, 0).GetObject(context.Background(), "b", "k")
	if !errors.Is(err, wantErr) {
		t.Fatalf("GetObject() error = %v, want it to wrap %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "s3://b/k") {
		t.Fatalf("GetObject() error = %v, want it to name the object", err)
	}
}

// Given no explicit size limit, when an object store is built, then the
// default applies.
func TestNewS3ObjectsAppliesDefaultLimit(t *testing.T) {
	if got := NewS3Objects(&fakeS3{}, 0).maxBytes; got != DefaultMaxObjectBytes {
		t.Fatalf("maxBytes = %d, want %d", got, DefaultMaxObjectBytes)
	}
}

// Given the ambient AWS configuration, when an object store is loaded, then it
// is built without contacting AWS.
func TestLoadS3Objects(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	objects, err := LoadS3Objects(context.Background(), "eu-west-1", 0)
	if err != nil {
		t.Fatalf("LoadS3Objects() error = %v", err)
	}
	if objects.api == nil {
		t.Fatal("LoadS3Objects() returned a store with no client")
	}
}
