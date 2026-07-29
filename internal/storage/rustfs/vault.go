// Package rustfs wraps Tier 1, the immutable object vault and sole source of
// truth for document content (ingestion-design.md §5.1).
package rustfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/suankan/pocket-advisor/internal/config"
	"github.com/suankan/pocket-advisor/internal/domain"
)

// Provenance is what travels with the bytes. Content-addressed keys carry no
// filename and the system never reads a user filesystem, so this metadata is
// the only record of where an object came from (§5.1).
type Provenance struct {
	SourceFilename string
	SourcePath     string
	CollectionID   string
	UploadedAt     string
	UploaderRunID  string
	AliasFilenames []string
	// Extra carries registry attributes the workspace config knows and the
	// bytes do not — ingestion type, and for bank collections the account
	// identification. Keys are stored verbatim under x-amz-meta-.
	Extra map[string]string
}

const (
	metaFilename = "source-filename"
	metaPath     = "source-path"
	metaColl     = "collection-id"
	metaUploaded = "uploaded-at"
	metaRunID    = "uploader-run-id"
	metaAliases  = "alias-filenames"
)

// encodeAliases serialises the alias list as JSON.
//
// It used to join on \x1f. That is a control character, which net/http rejects
// outright in a header value — so the moment an object acquired a second alias,
// every subsequent metadata write on it failed permanently with "invalid header
// field value". JSON escapes control characters as \uXXXX, so any filename the
// filesystem allows survives the round trip.
func encodeAliases(aliases []string) string {
	if len(aliases) == 0 {
		return ""
	}
	b, err := json.Marshal(aliases)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeAliases reads either form. Objects written before the JSON change still
// carry \x1f-joined values, and re-reading them must not lose the names.
func decodeAliases(s string) []string {
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "[") {
		var out []string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
	}
	return strings.Split(s, "\x1f")
}

func (p Provenance) toUserMetadata() map[string]string {
	m := map[string]string{
		metaFilename: p.SourceFilename,
		metaPath:     p.SourcePath,
		metaColl:     p.CollectionID,
		metaUploaded: p.UploadedAt,
		metaRunID:    p.UploaderRunID,
	}
	if a := encodeAliases(p.AliasFilenames); a != "" {
		m[metaAliases] = a
	}
	for k, v := range p.Extra {
		if v == "" {
			continue
		}
		// Never let an Extra key shadow a core provenance field.
		if _, reserved := m[k]; !reserved {
			m[k] = v
		}
	}
	return m
}

// decodeWord reverses the RFC 2047 encoding minio-go applies to non-ASCII
// metadata values on the way out.
//
// It encodes on write but does not decode on read, so a Cyrillic filename comes
// back as "=?UTF-8?B?...?=". Left alone, that value never equals the filename it
// was made from — which made the uploader believe every non-ASCII document had
// been renamed, and record a fresh alias for it on every single run.
func decodeWord(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	if decoded, err := (&mime.WordDecoder{}).DecodeHeader(s); err == nil {
		return decoded
	}
	return s
}

func provenanceFrom(userMeta map[string]string) Provenance {
	// minio-go returns user metadata with the X-Amz-Meta- prefix stripped but
	// canonicalised, so look keys up case-insensitively.
	get := func(k string) string {
		for name, v := range userMeta {
			if strings.EqualFold(strings.TrimPrefix(name, "X-Amz-Meta-"), k) {
				return decodeWord(v)
			}
		}
		return ""
	}
	p := Provenance{
		SourceFilename: get(metaFilename),
		SourcePath:     get(metaPath),
		CollectionID:   get(metaColl),
		UploadedAt:     get(metaUploaded),
		UploaderRunID:  get(metaRunID),
	}
	p.AliasFilenames = decodeAliases(get(metaAliases))

	// Anything not a core field is a registry attribute. Round-tripping it
	// matters: AddAlias rewrites the object's metadata wholesale, so a key
	// dropped here is a key deleted from Tier 1.
	core := map[string]bool{
		metaFilename: true, metaPath: true, metaColl: true,
		metaUploaded: true, metaRunID: true, metaAliases: true,
	}
	for name, v := range userMeta {
		k := strings.ToLower(strings.TrimPrefix(name, "X-Amz-Meta-"))
		if core[k] || v == "" {
			continue
		}
		if p.Extra == nil {
			p.Extra = map[string]string{}
		}
		p.Extra[k] = v
	}
	return p
}

// Vault is the Tier 1 client.
type Vault struct {
	c      *minio.Client
	bucket string
}

// NewUploader returns a client bound to the uploader identity: the only one
// allowed to write raw/ and the only one allowed to delete.
func NewUploader(cfg config.RustFS) (*Vault, error) {
	return newVault(cfg, cfg.UploaderAccessKey, cfg.UploaderSecretKey)
}

// NewWorker returns a client bound to the worker identity: reads anywhere,
// writes only under extracted/.
//
// One process now performs both roles, so the split is no longer implied by
// which binary is running. Keeping two clients is what keeps RustFS enforcing
// it rather than this code promising it (§5.1).
func NewWorker(cfg config.RustFS) (*Vault, error) {
	return newVault(cfg, cfg.WorkerAccessKey, cfg.WorkerSecretKey)
}

func newVault(cfg config.RustFS, accessKey, secretKey string) (*Vault, error) {
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}
	return &Vault{c: c, bucket: cfg.Bucket}, nil
}

// EnsureBucket creates the bucket if it does not exist. Safe to call from
// every process at startup.
func (v *Vault) EnsureBucket(ctx context.Context) error {
	ok, err := v.c.BucketExists(ctx, v.bucket)
	if err != nil {
		return fmt.Errorf("bucket exists check: %w", err)
	}
	if ok {
		return nil
	}
	if err := v.c.MakeBucket(ctx, v.bucket, minio.MakeBucketOptions{}); err != nil {
		// Concurrent creation is not an error.
		if exists, checkErr := v.c.BucketExists(ctx, v.bucket); checkErr == nil && exists {
			return nil
		}
		return fmt.Errorf("make bucket %q: %w", v.bucket, err)
	}
	return nil
}

// Exists reports whether an object is already present. This is the uploader's
// skip-if-present check (§5.1): exact rather than heuristic, because the key
// is the content hash.
func (v *Vault) Exists(ctx context.Context, key string) (bool, Provenance, error) {
	info, err := v.c.StatObject(ctx, v.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).StatusCode == 404 {
			return false, Provenance{}, nil
		}
		return false, Provenance{}, fmt.Errorf("stat %q: %w", key, err)
	}
	return true, provenanceFrom(info.UserMetadata), nil
}

// Put writes an object with its provenance attached.
func (v *Vault) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string, p Provenance) error {
	_, err := v.c.PutObject(ctx, v.bucket, key, r, size, minio.PutObjectOptions{
		ContentType:  contentType,
		UserMetadata: p.toUserMetadata(),
	})
	if err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}
	return nil
}

// AddAlias records an additional filename for content already stored. The
// object is not rewritten — one content, one object; the alias is kept because
// a second name is sometimes evidence in itself (§5.1).
func (v *Vault) AddAlias(ctx context.Context, key, filename string) error {
	exists, p, err := v.Exists(ctx, key)
	if err != nil || !exists {
		return err
	}
	if filename == "" || filename == p.SourceFilename {
		return nil
	}
	for _, a := range p.AliasFilenames {
		if a == filename {
			return nil
		}
	}
	p.AliasFilenames = append(p.AliasFilenames, filename)

	src := minio.CopySrcOptions{Bucket: v.bucket, Object: key}
	dst := minio.CopyDestOptions{
		Bucket:          v.bucket,
		Object:          key,
		UserMetadata:    p.toUserMetadata(),
		ReplaceMetadata: true,
	}
	if _, err := v.c.CopyObject(ctx, dst, src); err != nil {
		return fmt.Errorf("add alias to %q: %w", key, err)
	}
	return nil
}

// Get reads an object fully into memory. Every consumer of Tier 1 in this
// system processes in RAM, so there is no streaming variant by design.
func (v *Vault) Get(ctx context.Context, key string) ([]byte, Provenance, error) {
	obj, err := v.c.GetObject(ctx, v.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, Provenance{}, fmt.Errorf("get %q: %w", key, err)
	}
	defer obj.Close()

	info, err := obj.Stat()
	if err != nil {
		return nil, Provenance{}, fmt.Errorf("stat %q: %w", key, err)
	}
	b, err := io.ReadAll(obj)
	if err != nil {
		return nil, Provenance{}, fmt.Errorf("read %q: %w", key, err)
	}
	return b, provenanceFrom(info.UserMetadata), nil
}

// ObjectRef is one entry from a listing.
type ObjectRef struct {
	Key  string
	Size int64
}

// List enumerates a prefix. Used by the discovery bucket scan, whose
// invariant is that every object under raw/ has a Tier 2 row (§5.2).
func (v *Vault) List(ctx context.Context, prefix string) ([]ObjectRef, error) {
	var out []ObjectRef
	for o := range v.c.ListObjects(ctx, v.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if o.Err != nil {
			return nil, fmt.Errorf("list %q: %w", prefix, o.Err)
		}
		out = append(out, ObjectRef{Key: o.Key, Size: o.Size})
	}
	return out, nil
}

// RemovePrefix deletes every object under a prefix. Only the uploader calls
// this, and only as part of a reset that also cascades into Tier 2 (§5.1).
func (v *Vault) RemovePrefix(ctx context.Context, prefix string) (int, error) {
	objects := make(chan minio.ObjectInfo)
	go func() {
		defer close(objects)
		for o := range v.c.ListObjects(ctx, v.bucket, minio.ListObjectsOptions{
			Prefix:    prefix,
			Recursive: true,
		}) {
			if o.Err != nil {
				continue
			}
			select {
			case objects <- o:
			case <-ctx.Done():
				return
			}
		}
	}()

	removed := 0
	var firstErr error
	for e := range v.c.RemoveObjects(ctx, v.bucket, objects, minio.RemoveObjectsOptions{}) {
		if e.Err != nil && firstErr == nil {
			firstErr = fmt.Errorf("remove %q: %w", e.ObjectName, e.Err)
		}
	}
	if firstErr != nil {
		return removed, firstErr
	}

	// RemoveObjects reports only failures, so count what is left instead.
	remaining, err := v.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	if len(remaining) > 0 {
		return 0, fmt.Errorf("%d objects remain under %q after removal", len(remaining), prefix)
	}
	return removed, nil
}

// Remove deletes a single object.
func (v *Vault) Remove(ctx context.Context, key string) error {
	if err := v.c.RemoveObject(ctx, v.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("remove %q: %w", key, err)
	}
	return nil
}

// URI renders the canonical s3:// form recorded in Tier 2.
func (v *Vault) URI(key string) string {
	return "s3://" + v.bucket + "/" + key
}

// KeyFromURI is the inverse of URI.
func (v *Vault) KeyFromURI(uri string) (string, error) {
	want := "s3://" + v.bucket + "/"
	if !strings.HasPrefix(uri, want) {
		return "", fmt.Errorf("uri %q is not in bucket %q", uri, v.bucket)
	}
	return strings.TrimPrefix(uri, want), nil
}

var _ = domain.RawObjectKey // keep the key helpers discoverable from here
