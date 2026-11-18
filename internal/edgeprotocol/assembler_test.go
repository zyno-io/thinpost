package edgeprotocol

import (
	"bytes"
	"crypto/sha256"
	"testing"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
)

func startFor(data []byte) *controlv1.DeliveryStart {
	digest := sha256.Sum256(data)
	return &controlv1.DeliveryStart{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, ExpectedSize: uint64(len(data)), Sha256: digest[:]}
}
func TestAssemblerVerifiesOrderedBoundedChunks(t *testing.T) {
	data := []byte("signed message")
	a, err := NewAssembler(startFor(data))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Add(&controlv1.DataChunk{Id: "attempt", Offset: 0, Data: data[:5]})
	if err != nil || got != nil {
		t.Fatal("first chunk")
	}
	got, err = a.Add(&controlv1.DataChunk{Id: "attempt", Offset: 5, Data: data[5:], Last: true})
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("final chunk")
	}
	if _, err = a.Add(&controlv1.DataChunk{Id: "attempt", Last: true, Data: []byte("x")}); err == nil {
		t.Fatal("accepted data after completion")
	}
}
func TestAssemblerRejectsMalformedSequences(t *testing.T) {
	data := []byte("signed message")
	for _, tc := range []struct {
		name  string
		chunk *controlv1.DataChunk
	}{
		{"wrong ID", &controlv1.DataChunk{Id: "other", Data: data, Last: true}},
		{"wrong offset", &controlv1.DataChunk{Id: "attempt", Offset: 1, Data: data, Last: true}},
		{"premature last", &controlv1.DataChunk{Id: "attempt", Data: data[:1], Last: true}},
		{"missing last", &controlv1.DataChunk{Id: "attempt", Data: data}},
		{"digest mismatch", &controlv1.DataChunk{Id: "attempt", Data: bytes.Repeat([]byte("x"), len(data)), Last: true}},
		{"oversized chunk", &controlv1.DataChunk{Id: "attempt", Data: make([]byte, MaxChunkBytes+1), Last: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := NewAssembler(startFor(data))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.Add(tc.chunk); err == nil {
				t.Fatal("accepted invalid chunk")
			}
		})
	}
	bad := startFor(data)
	bad.ExpectedSize = MaxMIMEBytes + 1
	if _, err := NewAssembler(bad); err == nil {
		t.Fatal("accepted oversized MIME")
	}
}

func TestDeliveryDigestBindsTrackingCutoff(t *testing.T) {
	start := startFor([]byte("signed message"))
	start.TrackingSendCutoffUnixMs = 1900000000000
	first, err := DeliveryDigest(start)
	if err != nil {
		t.Fatal(err)
	}
	start.TrackingSendCutoffUnixMs++
	second, err := DeliveryDigest(start)
	if err != nil || first == second {
		t.Fatal("tracking cutoff was not bound to the permit", err)
	}
	start.TrackingSendCutoffUnixMs = -1
	if _, err = DeliveryDigest(start); err == nil {
		t.Fatal("negative tracking cutoff accepted")
	}
}
