package objectstore

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestCollectorListsBoundedPagesAndPreservesRecentObjects(t *testing.T) {
	const prefix = "final/v1/1000000000/"
	a, b, c := prefix+"00000000000000000000000000000001", prefix+"00000000000000000000000000000002", prefix+"00000000000000000000000000000003"
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	recent := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	objects := map[string]time.Time{a: old, b: old, c: recent}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/thinpost/" && r.Method == http.MethodGet {
			q := r.URL.Query()
			limit, _ := strconv.Atoi(q.Get("max-keys"))
			if q.Get("prefix") != "final/v1/" || limit < 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var keys []string
			for key := range objects {
				if strings.HasPrefix(key, q.Get("prefix")) && key > q.Get("start-after") {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			truncated := len(keys) > limit
			if truncated {
				keys = keys[:limit]
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>thinpost</Name><Prefix>final/v1/</Prefix><IsTruncated>%t</IsTruncated>`, truncated)
			for _, key := range keys {
				_, _ = fmt.Fprintf(w, `<Contents><Key>%s</Key><LastModified>%s</LastModified><ETag>"etag"</ETag><Size>1</Size></Contents>`, key, objects[key].Format(time.RFC3339))
			}
			if truncated {
				_, _ = fmt.Fprint(w, `<NextContinuationToken>next</NextContinuationToken>`)
			}
			_, _ = fmt.Fprint(w, `</ListBucketResult>`)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/thinpost/")
		if key == prefix+"00000000000000000000000000000004" && r.Method == http.MethodHead {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		modified, ok := objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
			w.Header().Set("Content-Length", "1")
			w.Header().Set("ETag", `"etag"`)
		case http.MethodDelete:
			delete(objects, key)
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
	store := &CollectorStore{Client: client, Bucket: "thinpost"}
	keys, err := store.List(context.Background(), "final/v1/", "", 2)
	if err != nil || len(keys) != 2 || keys[0] != a || keys[1] != b {
		t.Fatal("incorrect bounded first page", keys, err)
	}
	keys, err = store.List(context.Background(), "final/v1/", b, 2)
	if err != nil || len(keys) != 1 || keys[0] != c {
		t.Fatal("incorrect second page", keys, err)
	}
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	if removed, err := store.RemoveOlderThan(context.Background(), c, cutoff); err != nil || removed {
		t.Fatal("recent upload was removed", err)
	}
	if removed, err := store.RemoveOlderThan(context.Background(), a, cutoff); err != nil || !removed {
		t.Fatal("old object was not removed", err)
	}
	if removed, err := store.RemoveOlderThan(context.Background(), a, cutoff); err != nil || removed {
		t.Fatal("missing object handling", err)
	}
	if _, err := store.RemoveOlderThan(context.Background(), "final/legacy", cutoff); err == nil {
		t.Fatal("legacy key accepted for deletion")
	}
	if _, err := store.RemoveOlderThan(context.Background(), prefix+"00000000000000000000000000000004", cutoff); err == nil {
		t.Fatal("unverifiable object age accepted for deletion")
	}
}
