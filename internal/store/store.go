// Package store defines the only door to BlobFish data: the Store and
// Buckets interfaces, their value types and their sentinel errors.
//
// Frontends (native /v1 API, web UI, a future S3 module) depend on this
// package only. They never import the engine, bbolt or touch files.
package store

import (
	"context"
	"errors"
	"io"
	"time"
)

// Store reads and writes objects inside existing buckets.
type Store interface {
	// Put streams r into bucket/key, replacing any existing object only once
	// the new one is fully durable. It never holds the whole body in memory.
	Put(ctx context.Context, bucket, key string, r io.Reader, meta Meta) (ObjectInfo, error)
	// Get opens an object for reading. rng selects a byte range; nil reads
	// the whole object, and a full read is checked against the stored
	// SHA-256 (a mismatch surfaces as ErrDataCorrupt instead of io.EOF).
	Get(ctx context.Context, bucket, key string, rng *Range) (io.ReadCloser, ObjectInfo, error)
	// Head returns object metadata without opening the data.
	Head(ctx context.Context, bucket, key string) (ObjectInfo, error)
	// Delete removes an object. Deleting a missing key is not an error.
	Delete(ctx context.Context, bucket, key string) error
	// List returns objects in key order, one page at a time.
	List(ctx context.Context, bucket string, opts ListOptions) (ListResult, error)
}

// Buckets manages the bucket namespace.
type Buckets interface {
	CreateBucket(ctx context.Context, name string) (BucketInfo, error)
	// DeleteBucket removes an empty bucket.
	DeleteBucket(ctx context.Context, name string) error
	ListBuckets(ctx context.Context) ([]BucketInfo, error)
}

// Meta is the caller-supplied metadata stored with an object.
type Meta struct {
	// ContentType defaults to application/octet-stream when empty.
	ContentType string
	// User holds X-Meta-* style pairs. Total size is capped by MaxUserMetaBytes.
	User map[string]string
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Bucket      string
	Key         string
	Size        int64
	ContentType string
	// ETag is the lowercase hex SHA-256 of the full object body.
	ETag     string
	Modified time.Time
	User     map[string]string
}

// BucketInfo describes a bucket.
type BucketInfo struct {
	Name    string
	Created time.Time
}

// ListOptions controls a List call.
type ListOptions struct {
	// Prefix limits results to keys starting with it.
	Prefix string
	// Delimiter, when set, rolls up keys that contain it after Prefix into
	// CommonPrefixes, like directories.
	Delimiter string
	// Cursor is ListResult.NextCursor from the previous page. Opaque.
	Cursor string
	// Limit caps Objects+CommonPrefixes per page. <=0 means DefaultListLimit;
	// values above MaxListLimit are clamped.
	Limit int
}

// ListResult is one page of a listing.
type ListResult struct {
	Objects        []ObjectInfo
	CommonPrefixes []string
	// NextCursor is empty when there are no more results.
	NextCursor string
}

// Limits shared by every frontend.
const (
	MaxKeyLen          = 1024
	MaxUserMetaBytes   = 2048
	DefaultListLimit   = 1000
	MaxListLimit       = 1000
	DefaultContentType = "application/octet-stream"
)

// Sentinel errors. Engines wrap these with %w; frontends map them to API
// error codes with errors.Is.
var (
	ErrNoSuchBucket      = errors.New("bucket not found")
	ErrNoSuchKey         = errors.New("object not found")
	ErrBucketExists      = errors.New("bucket already exists")
	ErrBucketNotEmpty    = errors.New("bucket is not empty")
	ErrInvalidBucketName = errors.New("invalid bucket name")
	ErrInvalidKey        = errors.New("invalid object key")
	ErrInvalidRange      = errors.New("range not satisfiable")
	ErrInvalidCursor     = errors.New("invalid list cursor")
	ErrMetadataTooLarge  = errors.New("user metadata too large")
	ErrDataCorrupt       = errors.New("stored data failed integrity check")
)
