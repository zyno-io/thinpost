package postgres

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/zyno-io/thinpost/internal/objectgc"
	"github.com/zyno-io/thinpost/internal/objectstore"
)

// This exercises the real sweep, restricted database role and S3 client
// together; the S3 service itself is an isolated in-memory HTTP fixture.
func testObjectCollectionEndToEnd(t *testing.T, repo *Repository, horizon time.Time, referenced, orphan, recentlyUploaded string) {
	t.Helper()
	modified := horizon.Add(-time.Hour).UTC().Truncate(time.Second)
	objects := map[string]bool{referenced: true, orphan: true, recentlyUploaded: true}
	ages := map[string]time.Time{referenced: modified, orphan: modified, recentlyUploaded: time.Now().UTC().Truncate(time.Second)}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/thinpost/" && r.Method == http.MethodGet {
			if r.URL.Query().Get("prefix") != "submissions/v1/" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprint(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>thinpost</Name><IsTruncated>false</IsTruncated>`)
			for _, key := range []string{referenced, orphan, recentlyUploaded} {
				if objects[key] {
					_, _ = fmt.Fprintf(w, `<Contents><Key>%s</Key><LastModified>%s</LastModified><ETag>"etag"</ETag><Size>1</Size></Contents>`, key, ages[key].Format(time.RFC3339))
				}
			}
			_, _ = fmt.Fprint(w, `</ListBucketResult>`)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/thinpost/")
		if !objects[key] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Last-Modified", ages[key].Format(http.TimeFormat))
			w.Header().Set("Content-Length", "1")
			w.Header().Set("ETag", `"etag"`)
		case http.MethodDelete:
			objects[key] = false
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("test", "test", ""), Secure: false, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	store := &objectstore.CollectorStore{Client: client, Bucket: "thinpost"}
	result, err := objectgc.Sweep(context.Background(), repo, store, "submissions", "", horizon, 10)
	if err != nil || !result.Done || result.Scanned != 3 || result.Kept != 2 || result.Deleted != 1 {
		t.Fatal("collector did not preserve reference and remove orphan", result.Scanned, result.Kept, result.Deleted, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !objects[referenced] || objects[orphan] || !objects[recentlyUploaded] {
		t.Fatal("collector changed unexpected objects")
	}
}
