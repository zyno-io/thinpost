package controlsession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/zyno-io/thinpost/internal/objectstore"
)

// pipelineRawObjects optionally uses a disposable local S3 emulator. It refuses
// hostnames and non-loopback IPs, ignores proxies, and uses synthetic credentials.
func pipelineRawObjects(t *testing.T) objectstore.RawObjects {
	t.Helper()
	endpoint := os.Getenv("THINPOST_TEST_S3_ENDPOINT")
	if endpoint == "" {
		return &integrationObjects{data: make(map[string][]byte)}
	}
	address, err := netip.ParseAddrPort(endpoint)
	if err != nil || !address.Addr().IsLoopback() || address.Addr().Zone() != "" || address.Port() == 0 {
		t.Fatal("S3 test endpoint must be a numeric loopback address and port")
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
		if target != endpoint {
			return nil, errors.New("S3 test attempted to dial outside its local endpoint")
		}
		return dialer.DialContext(ctx, network, target)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4("fixture-access", "fixture-secret", ""),
		Secure: false, Region: "us-east-1", Transport: transport,
	})
	if err != nil {
		t.Fatal("cannot configure local S3 test client", err)
	}
	var random [12]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "thinpost-fixture-" + hex.EncodeToString(random[:])
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatal("cannot create disposable S3 test bucket", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for object := range client.ListObjects(cleanupCtx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			if object.Err != nil {
				t.Errorf("list disposable S3 test bucket: %v", object.Err)
				return
			}
			if removeErr := client.RemoveObject(cleanupCtx, bucket, object.Key, minio.RemoveObjectOptions{}); removeErr != nil {
				t.Errorf("remove disposable S3 object: %v", removeErr)
				return
			}
		}
		if removeErr := client.RemoveBucket(cleanupCtx, bucket); removeErr != nil {
			t.Errorf("remove disposable S3 test bucket: %v", removeErr)
		}
	})
	return &objectstore.Store{Client: client, Bucket: bucket}
}
