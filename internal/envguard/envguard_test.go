package envguard

import (
	"errors"
	"testing"
)

func TestRefuseGRPCBinaryLog(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if err := RefuseGRPCBinaryLog(get); err != nil {
		t.Fatal(err)
	}
	env["GRPC_BINARY_LOG_FILTER"] = "*"
	if err := RefuseGRPCBinaryLog(get); !errors.Is(err, ErrGRPCBinaryLog) {
		t.Fatalf("expected refusal, got %v", err)
	}
}
