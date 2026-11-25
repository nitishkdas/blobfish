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
	// GetBucket returns ErrNoSuchBucket if name does not exist.
	GetBucket(ctx context.Context, name string) (BucketInfo, error)
	// SetBucketPublic makes a bucket's objects readable without a key (or
	// not). Listing and writing always need a key.
	SetBucketPublic(ctx context.Context, name string, public bool) (BucketInfo, error)
	// SetBucketCORS replaces the web origins whose pages may call this
	// bucket's objects from a browser. Empty = none.
	SetBucketCORS(ctx context.Context, name string, origins []string) (BucketInfo, error)
}

// Links stores share links: permanent or long-lived URLs to one object
// that can be revoked one by one. Deleting an object deletes its links.
type Links interface {
	CreateLink(ctx context.Context, rec LinkRecord) error
	// LinkByToken and LinkByID return ErrNoSuchLink when nothing matches.
	LinkByToken(ctx context.Context, token string) (LinkRecord, error)
	LinkByID(ctx context.Context, id string) (LinkRecord, error)
	// ListLinks returns links for one object (key set), one bucket (key
	// empty) or everything (bucket empty), oldest first.
	ListLinks(ctx context.Context, bucket, key string) ([]LinkRecord, error)
	DeleteLink(ctx context.Context, id string) error
}

// LinkRecord is a stored share link. The token is kept as-is (not hashed
// like API keys) so the UI can show a link again; anyone able to read the
// metadata file can read the object files on the same disk anyway.
type LinkRecord struct {
	ID     string `json:"id"`
	Token  string `json:"token"`
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	// Expires is zero for a link that never expires.
	Expires       time.Time `json:"expires,omitzero"`
	Created       time.Time `json:"created"`
	CreatedBy     string    `json:"createdBy"`     // API key ID
	CreatedByName string    `json:"createdByName"` // its name at the time
}

// HealthChecker runs a self-check of the data store.
type HealthChecker interface {
	Health(ctx context.Context) HealthReport
}

// HealthReport is the outcome of a self-check.
type HealthReport struct {
	Metadata  error // metadata database is readable
	Disk      error // data disk accepts a write and fsync
	FreeBytes int64 // free space on the data disk; -1 if unknown
	Reserve   int64 // configured free-space reserve
	// LowSpace: free space is at or below the reserve, so uploads are
	// refused (reads and deletes still work).
	LowSpace bool
}

// Backuper writes a consistent snapshot of everything (metadata and every
// referenced object) to w, without stopping reads or writes.
type Backuper interface {
	Backup(ctx context.Context, w io.Writer) error
}

// Keys persists API key records. Only the SHA-256 hash of a token is ever
// stored; the token itself is shown to its creator once and then forgotten.
type Keys interface {
	CreateKey(ctx context.Context, rec KeyRecord) error
	// KeyByHash returns ErrNoSuchAPIKey if no key has this token hash.
	KeyByHash(ctx context.Context, hash string) (KeyRecord, error)
	// ListKeys returns all keys, oldest first.
	ListKeys(ctx context.Context) ([]KeyRecord, error)
	// DeleteKey returns ErrNoSuchAPIKey if id is unknown.
	DeleteKey(ctx context.Context, id string) error
}

// KeyRecord is a stored API key.
type KeyRecord struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Scopes  []string  `json:"scopes"`
	Created time.Time `json:"created"`
	// Hash is the lowercase hex SHA-256 of the token.
	Hash string `json:"hash"`
}

// Meta is the caller-supplied metadata stored with an object.
type Meta struct {
	// ContentType defaults to application/octet-stream when empty.
	ContentType string
	// User holds X-Meta-* style pairs. Total size is capped by MaxUserMetaBytes.
	User map[string]string
	// SizeHint is the body size when known in advance (Content-Length), so
	// an upload that can't fit is refused before it streams. <= 0: unknown.
	// Not stored.
	SizeHint int64
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
	// Public buckets serve GET/HEAD of their objects without a key.
	Public bool
	// CORSOrigins are web origins ("https://app.example.com" or "*")
	// whose pages may call this bucket's objects from a browser.
	CORSOrigins []string
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
	ErrNoSuchAPIKey      = errors.New("API key not found")
	ErrNoSuchLink        = errors.New("share link not found")
	ErrInvalidOrigin     = errors.New("invalid web origin")
	// ErrInsufficientStorage: accepting the upload would cut into the free
	// space reserve kept so deletes and metadata updates always fit.
	ErrInsufficientStorage = errors.New("not enough free disk space")
)
