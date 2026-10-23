// Package backup makes encrypted backups of the Apex database, stores them in
// an S3-compatible bucket and restores them.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config points at a bucket in any S3-compatible service (AWS S3, Cloudflare
// R2, Backblaze B2, Wasabi, MinIO, ...).
type S3Config struct {
	Endpoint  string `json:"endpoint"`   // https://s3.us-east-1.amazonaws.com, https://<acct>.r2.cloudflarestorage.com, http://minio:9000
	Region    string `json:"region"`     // optional for most providers
	Bucket    string `json:"bucket"`     //
	Prefix    string `json:"prefix"`     // folder inside the bucket, e.g. "apex/"
	AccessKey string `json:"access_key"` //
	SecretKey string `json:"secret_key"` //
	PathStyle bool   `json:"path_style"` // MinIO and some others need path-style URLs
}

// Validate checks the config is complete.
func (c S3Config) Validate() error {
	switch {
	case strings.TrimSpace(c.Endpoint) == "":
		return errors.New("enter the S3 endpoint")
	case strings.TrimSpace(c.Bucket) == "":
		return errors.New("enter the bucket name")
	case c.AccessKey == "" || c.SecretKey == "":
		return errors.New("enter the access key and secret key")
	}
	if _, _, err := parseEndpoint(c.Endpoint); err != nil {
		return err
	}
	return nil
}

func parseEndpoint(ep string) (host string, secure bool, err error) {
	ep = strings.TrimSpace(ep)
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" {
		return "", false, fmt.Errorf("the endpoint %q is not a valid URL", ep)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", false, errors.New("the endpoint must start with https:// or http://")
	}
	if u.Path != "" && u.Path != "/" {
		return "", false, errors.New("the endpoint must be just the server address; put the bucket name in the Bucket field")
	}
	return u.Host, u.Scheme == "https", nil
}

// Object is a backup in the bucket.
type Object struct {
	Key      string    `json:"key"`  // full object key
	Name     string    `json:"name"` // key without the prefix
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Store reads and writes backups in a bucket.
type Store struct {
	c      *minio.Client
	bucket string
	prefix string
}

// NewStore creates a client (it does not contact the server).
func NewStore(cfg S3Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	host, secure, _ := parseEndpoint(cfg.Endpoint)
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure,
		Region: strings.TrimSpace(cfg.Region),
	}
	if cfg.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	c, err := minio.New(host, opts)
	if err != nil {
		return nil, err
	}
	return &Store{c: c, bucket: strings.TrimSpace(cfg.Bucket), prefix: normPrefix(cfg.Prefix)}, nil
}

func normPrefix(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// Key returns the object key for a backup file name.
func (s *Store) Key(name string) string { return s.prefix + name }

// Owns reports whether key is a backup inside this store's prefix.
func (s *Store) Owns(key string) bool {
	return strings.HasPrefix(key, s.prefix) && IsBackupName(strings.TrimPrefix(key, s.prefix))
}

// Test checks the bucket exists and the credentials can write, read and delete.
func (s *Store) Test(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		return friendly(err)
	}
	if !ok {
		return fmt.Errorf("bucket %q does not exist", s.bucket)
	}
	key := s.prefix + ".apex-write-test"
	body := "apex backup write test " + time.Now().UTC().Format(time.RFC3339)
	if _, err := s.c.PutObject(ctx, s.bucket, key, strings.NewReader(body), int64(len(body)), minio.PutObjectOptions{ContentType: "text/plain"}); err != nil {
		return fmt.Errorf("cannot write to the bucket: %w", friendly(err))
	}
	o, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err == nil {
		var b []byte
		b, err = io.ReadAll(o)
		o.Close()
		if err == nil && string(b) != body {
			err = errors.New("read back different content")
		}
	}
	if err != nil {
		return fmt.Errorf("cannot read from the bucket: %w", friendly(err))
	}
	if err := s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("cannot delete from the bucket (needed to remove old backups): %w", friendly(err))
	}
	return nil
}

// List returns the backups in the bucket, newest first.
func (s *Store) List(ctx context.Context) ([]Object, error) {
	out := []Object{}
	for o := range s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix}) {
		if o.Err != nil {
			return nil, friendly(o.Err)
		}
		name := strings.TrimPrefix(o.Key, s.prefix)
		if strings.Contains(name, "/") || !IsBackupName(name) {
			continue
		}
		out = append(out, Object{Key: o.Key, Name: name, Size: o.Size, Modified: o.LastModified})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Upload stores the file at localPath as name.
func (s *Store) Upload(ctx context.Context, name, localPath string) (int64, error) {
	info, err := s.c.FPutObject(ctx, s.bucket, s.prefix+name, localPath, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return 0, friendly(err)
	}
	return info.Size, nil
}

// Open streams a backup from the bucket.
func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	if !s.Owns(key) {
		return nil, 0, errors.New("not a backup in this bucket")
	}
	o, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, friendly(err)
	}
	st, err := o.Stat()
	if err != nil {
		o.Close()
		return nil, 0, friendly(err)
	}
	return o, st.Size, nil
}

// Delete removes a backup.
func (s *Store) Delete(ctx context.Context, key string) error {
	if !s.Owns(key) {
		return errors.New("not a backup in this bucket")
	}
	return friendly(s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}))
}

// Prune deletes all but the newest keep backups and returns the deleted keys.
func (s *Store) Prune(ctx context.Context, keep int) ([]string, error) {
	if keep < 1 {
		return nil, nil
	}
	list, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var gone []string
	for i := keep; i < len(list); i++ {
		if err := s.Delete(ctx, list[i].Key); err != nil {
			return gone, err
		}
		gone = append(gone, list[i].Key)
	}
	return gone, nil
}

const namePrefix, nameSuffix = "apex-backup-", ".tar.gz.age"

// NewName returns the file name for a backup taken at t.
func NewName(t time.Time) string {
	return namePrefix + t.UTC().Format("20060102T150405Z") + nameSuffix
}

// IsBackupName reports whether name looks like a file made by NewName.
func IsBackupName(name string) bool {
	if path.Base(name) != name || !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, nameSuffix) {
		return false
	}
	_, err := time.Parse("20060102T150405Z", strings.TrimSuffix(strings.TrimPrefix(name, namePrefix), nameSuffix))
	return err == nil
}

// friendly turns S3 error responses into short messages.
func friendly(err error) error {
	if err == nil {
		return nil
	}
	var r minio.ErrorResponse
	if errors.As(err, &r) && r.Code != "" {
		switch r.Code {
		case "AccessDenied":
			return errors.New("access denied - check the access key's permissions on this bucket")
		case "InvalidAccessKeyId":
			return errors.New("the access key is not recognised")
		case "SignatureDoesNotMatch":
			return errors.New("the secret key is wrong")
		case "NoSuchBucket":
			return errors.New("the bucket does not exist")
		case "AuthorizationHeaderMalformed", "AuthorizationQueryParametersError":
			if r.Region != "" {
				return fmt.Errorf("wrong region - this bucket is in %s", r.Region)
			}
		}
		if r.Message != "" {
			return fmt.Errorf("%s: %s", r.Code, r.Message)
		}
		return errors.New(r.Code)
	}
	return err
}
