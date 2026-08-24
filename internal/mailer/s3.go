package mailer

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// GetObjectAPI is the part of the S3 client the object store uses. Depending
// on an interface rather than the concrete client is what lets tests run the
// whole receive path without AWS.
type GetObjectAPI interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// DefaultMaxObjectBytes bounds a stored inbound message. SES will not accept a
// message larger than 40 MB, so anything bigger is not a message we wrote a
// receipt rule for.
const DefaultMaxObjectBytes = 40 << 20

// S3Objects reads inbound messages an SES receipt rule wrote to S3. It is safe
// for concurrent use.
type S3Objects struct {
	api      GetObjectAPI
	maxBytes int64
}

// NewS3Objects returns an ObjectGetter backed by api. maxBytes of zero means
// DefaultMaxObjectBytes.
func NewS3Objects(api GetObjectAPI, maxBytes int64) *S3Objects {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxObjectBytes
	}
	return &S3Objects{api: api, maxBytes: maxBytes}
}

// LoadS3Objects builds an object store from the ambient AWS configuration: the
// environment, the shared credentials files, or the instance role. It does not
// contact AWS, so a bad configuration surfaces on the first GetObject.
func LoadS3Objects(ctx context.Context, region string, maxBytes int64) (*S3Objects, error) {
	options := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		options = append(options, awsconfig.WithRegion(region))
	}
	loaded, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load aws configuration: %w", err)
	}
	return NewS3Objects(s3.NewFromConfig(loaded), maxBytes), nil
}

// GetObject reads one stored message, refusing to buffer more than maxBytes of
// what is, after all, a file a stranger sent us.
func (o *S3Objects) GetObject(ctx context.Context, bucket, key string) ([]byte, error) {
	out, err := o.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get s3://%s/%s: %w", bucket, key, err)
	}
	defer out.Body.Close()

	// One byte over the limit is read so that the difference between "exactly
	// at the limit" and "truncated" is visible rather than silent.
	data, err := io.ReadAll(io.LimitReader(out.Body, o.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read s3://%s/%s: %w", bucket, key, err)
	}
	if int64(len(data)) > o.maxBytes {
		return nil, fmt.Errorf("mailer: s3://%s/%s is over the %d byte limit", bucket, key, o.maxBytes)
	}
	return data, nil
}
