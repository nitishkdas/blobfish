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
	// SetBucketQuota caps the bucket's live bytes; 0 removes the cap. A
	// quota below current use is allowed: it only blocks growth.
	SetBucketQuota(ctx context.Context, name string, quotaBytes int64) (BucketInfo, error)
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

// Event types sent to webhooks.
const (
	EventObjectCreated  = "object.created"  // new object or overwrite
	EventObjectDeleted  = "object.deleted"  // deleted, or moved to trash
	EventObjectRestored = "object.restored" // put back from trash
	EventPing           = "ping"            // a test sent on request
)

// EventTypes are the types a webhook can subscribe to.
var EventTypes = []string{EventObjectCreated, EventObjectDeleted, EventObjectRestored}

// Webhooks stores per-bucket webhooks. Events are queued in the same
// transaction as the change they describe, so none is lost to a crash;
// delivery reads them from the Outbox.
type Webhooks interface {
	CreateWebhook(ctx context.Context, rec WebhookRecord) error
	// WebhookByID returns ErrNoSuchWebhook when nothing matches.
	WebhookByID(ctx context.Context, id string) (WebhookRecord, error)
	ListWebhooks(ctx context.Context, bucket string) ([]WebhookRecord, error)
	// DeleteWebhook removes the webhook and its undelivered events.
	DeleteWebhook(ctx context.Context, id string) error
	// QueueTestEvent queues a "ping" event for one webhook.
	QueueTestEvent(ctx context.Context, id string) error
}

// WebhookRecord is a stored webhook and its delivery health.
type WebhookRecord struct {
	ID     string   `json:"id"`
	Bucket string   `json:"bucket"`
	URL    string   `json:"url"`
	Secret string   `json:"secret"` // HMAC key for signatures; needed as-is
	Events []string `json:"events"`
	// AllowPrivate lets deliveries go to loopback and private addresses.
	// Set only for webhooks made by server admins.
	AllowPrivate  bool      `json:"allowPrivate,omitempty"`
	Created       time.Time `json:"created"`
	CreatedBy     string    `json:"createdBy"`
	CreatedByName string    `json:"createdByName"`

	Pending     int       `json:"pending"` // queued, not yet delivered
	Dropped     int       `json:"dropped"` // not queued: too many pending
	GaveUp      int       `json:"gaveUp"`  // failed every retry
	LastSuccess time.Time `json:"lastSuccess,omitzero"`
	LastFailure time.Time `json:"lastFailure,omitzero"`
	LastError   string    `json:"lastError,omitempty"`
	// RetryAfter pauses all deliveries to this webhook after a failure,
	// so events wait in order instead of each hitting a dead receiver.
	RetryAfter time.Time `json:"retryAfter,omitzero"`
}

// Outbox hands queued webhook events to the dispatcher.
type Outbox interface {
	// DueDeliveries returns up to limit deliveries whose time has come,
	// oldest first.
	DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error)
	DeliverySucceeded(ctx context.Context, seq uint64, at time.Time) error
	// DeliveryFailed records a failed attempt; retryAt zero means give up.
	DeliveryFailed(ctx context.Context, seq uint64, at time.Time, problem string, retryAt time.Time) error
	// EventSignal receives a value after events are queued.
	EventSignal() <-chan struct{}
}

// Delivery is one event on its way to one webhook.
type Delivery struct {
	Seq          uint64
	WebhookID    string
	URL          string
	Secret       string
	AllowPrivate bool
	EventID      string
	EventType    string
	Payload      []byte // the JSON body to send
	Attempts     int    // failed attempts so far
}

// UploadLinks stores upload links: URLs that let someone without a key
// add files to one folder of a bucket, never read or replace anything.
type UploadLinks interface {
	CreateUploadLink(ctx context.Context, rec UploadLinkRecord) error
	// UploadLinkByToken and UploadLinkByID return ErrNoSuchLink when
	// nothing matches.
	UploadLinkByToken(ctx context.Context, token string) (UploadLinkRecord, error)
	UploadLinkByID(ctx context.Context, id string) (UploadLinkRecord, error)
	// ListUploadLinks returns a bucket's upload links, oldest first.
	ListUploadLinks(ctx context.Context, bucket string) ([]UploadLinkRecord, error)
	DeleteUploadLink(ctx context.Context, id string) error
	// ClaimUploadSlot counts one upload against the link's file limit,
	// failing with ErrUploadLimitReached when none are left. Call
	// ReleaseUploadSlot if the upload then fails, so the slot is not lost.
	ClaimUploadSlot(ctx context.Context, token string) (UploadLinkRecord, error)
	ReleaseUploadSlot(ctx context.Context, token string) error
}

// UploadLinkRecord is a stored upload link.
type UploadLinkRecord struct {
	ID     string `json:"id"`
	Token  string `json:"token"`
	Bucket string `json:"bucket"`
	// Prefix is the folder files land in: "" or ending in "/".
	Prefix string `json:"prefix"`
	// Label is shown to the uploader, e.g. "Receipts for ACME".
	Label string `json:"label,omitempty"`
	// Expires is zero for a link that never expires.
	Expires time.Time `json:"expires,omitzero"`
	// MaxFiles caps uploads through the link (0 = no cap); Uploads counts
	// them. MaxFileSize caps each file in bytes (0 = the server's limit).
	MaxFiles      int       `json:"maxFiles,omitempty"`
	Uploads       int       `json:"uploads"`
	MaxFileSize   int64     `json:"maxFileSize,omitempty"`
	Created       time.Time `json:"created"`
	CreatedBy     string    `json:"createdBy"`
	CreatedByName string    `json:"createdByName"`
}

// Trash keeps deleted objects for a while so they can be restored. While
// an object is in trash, its share links don't work; restoring it brings
// them back.
type Trash interface {
	// TrashRetention is how long a deleted object is kept; 0 means trash
	// is off and Delete removes objects at once.
	TrashRetention() time.Duration
	// ListTrash lists one bucket's trash, most recently deleted first.
	ListTrash(ctx context.Context, bucket string, opts TrashListOptions) (TrashList, error)
	// RestoreTrash puts a trashed object back under its key. It fails with
	// ErrKeyExists if another object has that key now.
	RestoreTrash(ctx context.Context, bucket, id string) (ObjectInfo, error)
	// PurgeTrash deletes one trashed object for good, or the bucket's
	// whole trash when id is empty.
	PurgeTrash(ctx context.Context, bucket, id string) error
}

// TrashItem is one deleted object waiting in trash.
type TrashItem struct {
	ID      string
	Object  ObjectInfo
	Deleted time.Time
	PurgeAt time.Time // when it is deleted for good
}

// TrashListOptions pages through a bucket's trash.
type TrashListOptions struct {
	Cursor string // NextCursor of the previous page
	Limit  int    // 0 = DefaultListLimit
}

// TrashList is one page of trash.
type TrashList struct {
	Items      []TrashItem
	NextCursor string
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
	// Filename is the name the file is served and saved under; empty means
	// the last part of the key. Older binaries ignore it (they fall back
	// to the key), so adding it needed no format version bump.
	Filename string `json:"filename,omitempty"`
	// Trashed is the trash ID of the deleted object this link points to;
	// set, the link does not work until the object is restored. Kept per
	// link so a new upload to the same key never revives old links.
	Trashed string `json:"trashed,omitempty"`
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
	// KeyByID returns ErrNoSuchAPIKey if no key has this id.
	KeyByID(ctx context.Context, id string) (KeyRecord, error)
	// ListKeys returns all keys, oldest first.
	ListKeys(ctx context.Context) ([]KeyRecord, error)
	// DeleteKey deletes the key with this id, or returns ErrNoSuchAPIKey.
	// If allow is not nil it runs in the same transaction, with the key
	// and every key that exists, and an error from it cancels the delete:
	// a check like "not the last admin key" can't race another delete.
	DeleteKey(ctx context.Context, id string, allow func(target KeyRecord, all []KeyRecord) error) error
}

// Users stores people who sign in with a name and password, and their
// sessions. Passwords and session tokens are stored only as hashes.
type Users interface {
	// CreateUser returns ErrUserExists if the name is taken, ErrEmailTaken
	// if rec.Email is another user's.
	CreateUser(ctx context.Context, rec UserRecord) error
	// GetUser returns ErrNoSuchUser if name is unknown.
	GetUser(ctx context.Context, name string) (UserRecord, error)
	// UserByEmail returns the user with this (normalized) email, or
	// ErrNoSuchUser.
	UserByEmail(ctx context.Context, email string) (UserRecord, error)
	// ListUsers returns all users, oldest first.
	ListUsers(ctx context.Context) ([]UserRecord, error)
	// UpdateUser changes a user in one transaction. When revokeSessions is
	// true it also deletes the user's sessions, except the one whose hash
	// is keepSession (may be empty), in the same transaction. A changed
	// Email must not be another user's (ErrEmailTaken).
	UpdateUser(ctx context.Context, name string, change func(*UserRecord) error, revokeSessions bool, keepSession string) (UserRecord, error)
	// DeleteUser deletes the user and all their sessions.
	DeleteUser(ctx context.Context, name string) error

	// CreateSession stores a session and records the sign-in time on its
	// user, in one transaction. ErrNoSuchUser if the user is gone;
	// ErrUserChanged if the user's password hash is no longer
	// passwordHash (the one the sign-in checked).
	CreateSession(ctx context.Context, rec SessionRecord, passwordHash string) error
	// SessionByHash returns the session and its user, or ErrNoSuchSession.
	SessionByHash(ctx context.Context, hash string) (SessionRecord, UserRecord, error)
	// TouchSession sets a session's LastSeen (no error if it is gone).
	TouchSession(ctx context.Context, hash string, t time.Time) error
	// DeleteSession deletes one session (no error if it is gone).
	DeleteSession(ctx context.Context, hash string) error
	// DeleteUserSessions signs a user out everywhere; returns how many.
	DeleteUserSessions(ctx context.Context, name string) (int, error)
	// PurgeSessions deletes sessions for which expired returns true.
	PurgeSessions(ctx context.Context, expired func(SessionRecord) bool) (int, error)
}

// UserRecord is a stored user.
type UserRecord struct {
	Name string `json:"name"`
	// Email is optional, normalized (lowercase), unique among users and
	// may change; Name is the fixed identity. Nothing is sent to it yet.
	Email  string   `json:"email,omitempty"`
	Scopes []string `json:"scopes"`
	// PasswordHash is "pbkdf2-sha256$<iterations>$<salt b64>$<hash b64>".
	PasswordHash    string    `json:"passwordHash"`
	Created         time.Time `json:"created"`
	PasswordChanged time.Time `json:"passwordChanged"`
	LastLogin       time.Time `json:"lastLogin,omitzero"`
}

// SessionRecord is a signed-in browser. Hash is the lowercase hex SHA-256
// of the token; ID is a short public handle for logs.
type SessionRecord struct {
	ID       string    `json:"id"`
	Hash     string    `json:"hash"`
	User     string    `json:"user"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"lastSeen"`
	// Expires is the absolute end, however active the session is.
	Expires time.Time `json:"expires"`
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
	// NoOverwrite makes Put fail with ErrKeyExists instead of replacing an
	// existing object; decided in the commit transaction, so it holds
	// against concurrent uploads. Not stored.
	NoOverwrite bool
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
	// QuotaBytes caps the bytes of the bucket's live objects; 0 = no
	// limit. Trash does not count.
	QuotaBytes int64
	// UsedBytes and Objects count the live objects (not trash).
	UsedBytes int64
	Objects   int64
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
	ErrNoSuchTrashItem   = errors.New("object not found in trash")
	// ErrKeyExists: a trashed object can't be restored because another
	// object has its key now.
	ErrKeyExists = errors.New("an object with this key already exists")
	// ErrInsufficientStorage: accepting the upload would cut into the free
	// space reserve kept so deletes and metadata updates always fit.
	ErrInsufficientStorage = errors.New("not enough free disk space")
	// ErrQuotaExceeded: the write would take the bucket over its quota.
	ErrQuotaExceeded = errors.New("bucket quota exceeded")
	ErrInvalidQuota  = errors.New("invalid quota")
	ErrNoSuchUser    = errors.New("no such user")
	ErrUserExists    = errors.New("user already exists")
	ErrEmailTaken    = errors.New("another user has this email")
	ErrNoSuchSession = errors.New("no such session")
	// ErrUserChanged: the user's password changed (or the user was made
	// again) while a sign-in was checking the old one.
	ErrUserChanged = errors.New("user changed during sign-in")
	// ErrCommitUncertain: storing failed in a way that leaves it unknown
	// whether the write took effect (the metadata commit reported an I/O
	// error). Callers must not assume nothing was stored.
	ErrCommitUncertain = errors.New("write may or may not have been stored")
	// ErrUploadLimitReached: an upload link has taken all the files it may.
	ErrUploadLimitReached = errors.New("upload link has no uploads left")
	ErrNoSuchWebhook      = errors.New("webhook not found")
	// ErrTooManyWebhooks: a bucket has MaxWebhooksPerBucket already.
	ErrTooManyWebhooks = errors.New("too many webhooks for this bucket")
)
