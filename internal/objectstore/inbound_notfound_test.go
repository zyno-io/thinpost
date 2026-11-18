package objectstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// IsNotFound is true only for a missing object, never for other storage errors,
// so the inbound dispatcher fails permanently only when the content is gone.
func TestIsNotFoundOnlyForAMissingObject(t *testing.T) {
	for code, want := range map[string]bool{"NoSuchKey": true, "AccessDenied": false, "InternalError": false} {
		status := map[string]int{"NoSuchKey": 404, "AccessDenied": 403, "InternalError": 500}[code]
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(status)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>x</Message><Key>k</Key><RequestId>1</RequestId></Error>`))
			}
		}))
		client, err := minio.New(strings.TrimPrefix(srv.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("a", "b", ""), Secure: false, Region: "us-east-1"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = (&Store{Client: client, Bucket: "bucket"}).Get(context.Background(), "inbound/x", 100)
		srv.Close()
		if err == nil || IsNotFound(err) != want {
			t.Errorf("%s: IsNotFound = %v, want %v (%v)", code, IsNotFound(err), want, err)
		}
	}
	if IsNotFound(nil) || IsNotFound(errors.New("plain")) {
		t.Error("non-storage errors are not 'not found'")
	}
}
