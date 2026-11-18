package controlsession

import (
	"crypto/sha256"
	"testing"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Schema-40 mixed-version contract. The messages below are declared as an older
// build declares them, without the sending-IP fields.
func field(name string, number int32, kind descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Type: kind.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()}
}

func preS40(t *testing.T, name string, fields ...*descriptorpb.FieldDescriptorProto) *dynamicpb.Message {
	t.Helper()
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("preS40_" + name + ".proto"), Package: proto.String("preS40"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String(name), Field: fields}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return dynamicpb.NewMessage(file.Messages().ByName(protoreflect.Name(name)))
}

func oldReady(t *testing.T) *dynamicpb.Message {
	return preS40(t, "Ready",
		field("worker_id", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		field("epoch", 2, descriptorpb.FieldDescriptorProto_TYPE_UINT64),
		field("session_id", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		field("protocol_version", 4, descriptorpb.FieldDescriptorProto_TYPE_UINT32),
		field("journal_id", 5, descriptorpb.FieldDescriptorProto_TYPE_STRING),
	)
}

func oldDeliveryStart(t *testing.T) *dynamicpb.Message {
	return preS40(t, "DeliveryStart",
		field("attempt_id", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		field("delivery_id", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		field("epoch", 3, descriptorpb.FieldDescriptorProto_TYPE_UINT64),
		field("mx_name", 4, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		field("destination_ip", 5, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		field("envelope_from", 6, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		field("recipient", 7, descriptorpb.FieldDescriptorProto_TYPE_STRING),
		field("enhanced_protection", 8, descriptorpb.FieldDescriptorProto_TYPE_BOOL),
		field("expected_size", 9, descriptorpb.FieldDescriptorProto_TYPE_UINT64),
		field("sha256", 10, descriptorpb.FieldDescriptorProto_TYPE_BYTES),
		field("tracking_send_cutoff_unix_ms", 11, descriptorpb.FieldDescriptorProto_TYPE_INT64),
	)
}

func newStart(source, helo string) *controlv1.DeliveryStart {
	digest := sha256.Sum256([]byte("signed MIME"))
	return &controlv1.DeliveryStart{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 3, MxName: "mx.example", DestinationIp: "8.8.8.8", EnvelopeFrom: "b+token@bounce.example", Recipient: "a@example.net", ExpectedSize: 11, Sha256: digest[:], SourceAddress: source, HeloName: helo}
}

// An old Edge's Ready has no source lists; control reads it as a legacy worker
// and uses its own legacy capabilities.
func TestOldShapeReadyDecodesAsLegacy(t *testing.T) {
	old := oldReady(t)
	fields := old.Descriptor().Fields()
	old.Set(fields.ByName("worker_id"), protoreflect.ValueOfString("worker"))
	old.Set(fields.ByName("epoch"), protoreflect.ValueOfUint64(1))
	old.Set(fields.ByName("protocol_version"), protoreflect.ValueOfUint32(uint32(edgeprotocol.Version)))
	wire, err := proto.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var ready controlv1.Ready
	if err = proto.Unmarshal(wire, &ready); err != nil {
		t.Fatal(err)
	}
	caps, valid := capabilitiesFromReady(&ready, legacyCaps)
	if !valid || caps.Explicit || len(caps.Addresses) != 1 || caps.Addresses[0] != legacyCaps.Addresses[0] {
		t.Fatalf("an old Ready was not treated as legacy: %+v %v", caps, valid)
	}
	// And a new worker's Ready, decoded by an old control plane, only gains
	// unknown fields: every field it knows is intact.
	newWire, err := proto.Marshal(&controlv1.Ready{WorkerId: "worker", Epoch: 1, ProtocolVersion: uint32(edgeprotocol.Version), SourceAddresses: []string{"45.0.0.10"}, BounceDomains: []string{"b.example"}})
	if err != nil {
		t.Fatal(err)
	}
	decoded := oldReady(t)
	if err = proto.Unmarshal(newWire, decoded); err != nil || decoded.Get(decoded.Descriptor().Fields().ByName("worker_id")).String() != "worker" {
		t.Fatal("an old control plane cannot read a new Ready", err)
	}
}

// An old Edge keeps the fields it does not know and hashes the whole message,
// so it would obtain a valid permit for a start that names a source it cannot
// honor, and then send from its primary address with its own HELO. Control's
// gating (nothing is set for an Edge that did not report sources) is therefore
// the only guard. This test pins both halves: the digest binds the new fields,
// and an old Edge computes the same digest.
func TestOldEdgeWouldPermitNewDeliveryStartFields(t *testing.T) {
	withSource, without := newStart("45.0.0.10", "smtp.hosting.example"), newStart("", "")
	want, err := edgeprotocol.DeliveryDigest(withSource)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := edgeprotocol.DeliveryDigest(without)
	if err != nil || plain == want {
		t.Fatal("the permit digest does not bind the source fields", err)
	}
	wire, err := proto.Marshal(withSource)
	if err != nil {
		t.Fatal(err)
	}
	old := oldDeliveryStart(t)
	if err = proto.Unmarshal(wire, old); err != nil {
		t.Fatal(err)
	}
	reencoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(reencoded) != want {
		t.Fatal("an old Edge computes a different digest, so this test no longer shows why control must gate")
	}
}

// A start for an Edge that reported nothing is byte-identical to the schema-39
// one: control sets neither new field, so an old Edge hashes what it always did.
func TestLegacyStartIsIdenticalToTheOldWireShape(t *testing.T) {
	start := newStart("", "")
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	old := oldDeliveryStart(t)
	if err = proto.Unmarshal(wire, old); err != nil {
		t.Fatal(err)
	}
	if len(old.GetUnknown()) != 0 {
		t.Fatal("a legacy start carries fields an old Edge does not know")
	}
}
