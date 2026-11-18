package objectstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/zyno-io/thinpost/internal/objectkey"
)

// CollectorStore must use a separate, narrowly scoped list/delete identity.
// Never attach it to a normal control runtime or an unreviewed bucket.
type CollectorStore struct {
	Client *minio.Client
	Bucket string
}

func (s *CollectorStore) List(ctx context.Context, prefix, after string, limit int) ([]string, error) {
	if s == nil || s.Client == nil || s.Bucket == "" || limit < 1 || limit > 500 || (prefix != "submissions/v1/" && prefix != "final/v1/" && prefix != "bounces/v1/" && prefix != "tracking/v1/") || (after != "" && (!strings.HasPrefix(after, prefix) || len(after) > 1024)) {
		return nil, errors.New("invalid object listing request")
	}
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	keys := make([]string, 0, limit)
	objects := s.Client.ListObjects(listCtx, s.Bucket, minio.ListObjectsOptions{Prefix: prefix, StartAfter: after, Recursive: true, MaxKeys: limit})
	for object := range objects {
		if object.Err != nil {
			return nil, errors.New("object listing unavailable")
		}
		keys = append(keys, object.Key)
		if len(keys) == limit {
			break
		}
	}
	if ctx.Err() != nil {
		return nil, errors.New("object listing unavailable")
	}
	return keys, nil
}

func (s *CollectorStore) RemoveOlderThan(ctx context.Context, key string, cutoff time.Time) (bool, error) {
	if s == nil || s.Client == nil || s.Bucket == "" {
		return false, errors.New("object collection unavailable")
	}
	if _, err := objectkey.Parse(key); err != nil || cutoff.IsZero() {
		return false, errors.New("invalid collection candidate")
	}
	object, err := s.Client.StatObject(ctx, s.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		response := minio.ToErrorResponse(err)
		if response.StatusCode == 404 && (response.Code == "NoSuchKey" || response.Code == "NotFound") {
			return false, nil
		}
		return false, errors.New("object age cannot be verified")
	}
	if object.LastModified.IsZero() {
		return false, errors.New("object age cannot be verified")
	}
	if object.LastModified.After(cutoff) {
		return false, nil
	}
	if err := s.Client.RemoveObject(ctx, s.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return false, errors.New("object deletion not confirmed")
	}
	return true, nil
}
