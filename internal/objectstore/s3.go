// Package objectstore stores immutable source submissions in an S3-compatible
// bucket. The runtime wraps it in authenticated object encryption; bucket
// access, retention and backup remain operator duties.
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/minio/minio-go/v7"
)

type Store struct {
	Client *minio.Client
	Bucket string
}

func (s *Store) VerifyPresent(ctx context.Context, key string) error {
	if !validObjectKey(key) {
		return errors.New("invalid object key")
	}
	_, err := s.Client.StatObject(ctx, s.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return errors.New("object presence cannot be verified")
	}
	return nil
}

func (s *Store) Put(ctx context.Context, key string, data []byte) error {
	return s.put(ctx, key, data, "application/octet-stream")
}

func (s *Store) PutMIME(ctx context.Context, key string, data []byte) error {
	return s.put(ctx, key, data, "message/rfc822")
}

func (s *Store) put(ctx context.Context, key string, data []byte, contentType string) error {
	options := minio.PutObjectOptions{ContentType: contentType, DisableMultipart: true}
	options.SetMatchETagExcept("*")
	_, err := s.Client.PutObject(ctx, s.Bucket, key, bytes.NewReader(data), int64(len(data)), options)
	return err
}

func (s *Store) Get(ctx context.Context, key string, maximum int64) ([]byte, error) {
	if maximum <= 0 {
		return nil, errors.New("invalid object size limit")
	}
	object, err := s.Client.GetObject(ctx, s.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer object.Close()
	data, err := io.ReadAll(io.LimitReader(object, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("object exceeds size limit")
	}
	return data, nil
}

// IsNotFound reports whether err says the object does not exist (as opposed to
// a storage failure that says nothing about it).
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	response := minio.ToErrorResponse(err)
	return response.StatusCode == 404 && (response.Code == "NoSuchKey" || response.Code == "NotFound")
}
