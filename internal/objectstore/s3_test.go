package objectstore

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestPutUsesConditionalWriteAndReportsFailure(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/thinpost/submissions/test" || r.Header.Get("If-None-Match") != "*" {
			t.Errorf("unexpected request: %s %s %q", r.Method, r.URL.Path, r.Header.Get("If-None-Match"))
			w.WriteHeader(400)
			return
		}
		if body != nil {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("ETag", `"etag"`)
		w.WriteHeader(200)
	}))
	defer server.Close()
	client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("test", "test", ""), Secure: false, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{Client: client, Bucket: "thinpost"}
	if err = store.Put(context.Background(), "submissions/test", []byte("private source")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "private source") {
		t.Fatal("source bytes missing from signed request")
	}
	if err = store.Put(context.Background(), "submissions/test", []byte("overwrite")); err == nil || !strings.Contains(string(body), "private source") {
		t.Fatal("existing object overwritten")
	}
}

func TestGetEnforcesObjectSizeLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		w.Header().Set("ETag", `"etag"`)
		_, _ = w.Write([]byte("123456"))
	}))
	defer server.Close()
	client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("test", "test", ""), Secure: false, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{Client: client, Bucket: "thinpost"}
	if data, err := store.Get(context.Background(), "source", 6); err != nil || string(data) != "123456" {
		t.Fatalf("object: %q %v", data, err)
	}
	if _, err := store.Get(context.Background(), "source", 5); err == nil {
		t.Fatal("oversized object accepted")
	}
}

func TestEncryptedStoreUsesOpaqueConditionalS3Objects(t *testing.T) {
	var stored []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/thinpost/submissions/abc" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodPut:
			if r.Header.Get("If-None-Match") != "*" || r.Header.Get("Content-Type") != "application/octet-stream" || stored != nil {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			var err error
			stored, err = readTestS3Body(r)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("ETag", `"etag"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("Content-Length", fmt.Sprint(len(stored)))
			w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
			w.Header().Set("ETag", `"etag"`)
			_, _ = w.Write(stored)
		case http.MethodHead:
			if stored == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(stored)))
			w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
			w.Header().Set("ETag", `"etag"`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{Creds: credentials.NewStaticV4("test", "test", ""), Secure: false, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEncrypted(&Store{Client: client, Bucket: "thinpost"}, testRing(t, "v1", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.VerifyPresent(context.Background(), "submissions/abc"); err == nil {
		t.Fatal("missing S3 object was reported present")
	}
	plaintext := []byte("sensitive test message")
	if err = store.PutMIME(context.Background(), "submissions/abc", plaintext); err != nil {
		t.Fatal(err)
	}
	if err = store.VerifyPresent(context.Background(), "submissions/abc"); err != nil {
		t.Fatal("stored S3 object was not verified", err)
	}
	if strings.Contains(string(stored), string(plaintext)) {
		t.Fatal("S3 received plaintext")
	}
	got, err := store.Get(context.Background(), "submissions/abc", int64(len(plaintext)))
	if err != nil || string(got) != string(plaintext) {
		t.Fatalf("encrypted S3 round trip failed (stored bytes=%d): %v", len(stored), err)
	}
	if err = store.Put(context.Background(), "submissions/abc", plaintext); err == nil {
		t.Fatal("S3 conditional write allowed replacement")
	}
}

// The production S3 service decodes AWS streaming chunks before storing the
// object. The test server must model that wire behavior as well.
func readTestS3Body(r *http.Request) ([]byte, error) {
	if r.Header.Get("Content-Encoding") != "aws-chunked" {
		return io.ReadAll(r.Body)
	}
	reader := bufio.NewReader(r.Body)
	var out bytes.Buffer
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeText := strings.SplitN(strings.TrimSpace(line), ";", 2)[0]
		size, err := strconv.ParseInt(sizeText, 16, 32)
		if err != nil || size < 0 || size > 1<<20 {
			return nil, fmt.Errorf("invalid test S3 chunk")
		}
		if size == 0 {
			return out.Bytes(), nil
		}
		if _, err = io.CopyN(&out, reader, size); err != nil {
			return nil, err
		}
		ending := make([]byte, 2)
		if _, err = io.ReadFull(reader, ending); err != nil || !bytes.Equal(ending, []byte("\r\n")) {
			return nil, fmt.Errorf("invalid test S3 chunk ending")
		}
	}
}
