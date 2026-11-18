package edgesession

import (
	"testing"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/journal"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// preS34Result is DeliveryResult as an older build declares it: fields 1-5 only.
// An old control plane decodes a new worker's result into this shape, keeps the
// unknown fields 6 and 7, and echoes the same message in the acknowledgement.
func preS34Result(t *testing.T) *dynamicpb.Message {
	t.Helper()
	field := func(name string, number int32, kind descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Type: kind.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()}
	}
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("preS34.proto"), Package: proto.String("preS34"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("DeliveryResult"), Field: []*descriptorpb.FieldDescriptorProto{
			field("attempt_id", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING),
			field("delivery_id", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING),
			field("epoch", 3, descriptorpb.FieldDescriptorProto_TYPE_UINT64),
			field("outcome", 4, descriptorpb.FieldDescriptorProto_TYPE_INT32),
			field("smtp_code", 5, descriptorpb.FieldDescriptorProto_TYPE_UINT32),
		}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return dynamicpb.NewMessage(file.Messages().ByName("DeliveryResult"))
}

func newResult() *controlv1.DeliveryResult {
	return &controlv1.DeliveryResult{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 3, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED, SmtpCode: 550, SmtpStage: controlv1.SmtpStage_SMTP_STAGE_RCPT, SmtpEnhanced: "5.1.1"}
}

// New Edge with an old control plane: the unknown fields survive the old
// control plane's decode and re-encode, so the exact-match acknowledgement holds.
func TestNewWorkerResultSurvivesOldControlEcho(t *testing.T) {
	sent := newResult()
	wire, err := proto.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	old := preS34Result(t)
	if err = proto.Unmarshal(wire, old); err != nil {
		t.Fatal(err)
	}
	// The old control plane understands its own fields...
	if old.Get(old.Descriptor().Fields().ByName("smtp_code")).Uint() != 550 {
		t.Fatal("old shape lost a known field")
	}
	// ...and echoes the whole message.
	echo, err := proto.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var acked controlv1.DeliveryResult
	if err = proto.Unmarshal(echo, &acked); err != nil {
		t.Fatal(err)
	}
	if !sameResult(sent, &acked) {
		t.Fatalf("acknowledgement of a new result through an old control plane does not match: %v vs %v", sent, &acked)
	}
	// A result with a different stage or code is still rejected.
	for _, change := range []func(*controlv1.DeliveryResult){
		func(r *controlv1.DeliveryResult) { r.SmtpStage = controlv1.SmtpStage_SMTP_STAGE_DATA },
		func(r *controlv1.DeliveryResult) { r.SmtpEnhanced = "5.1.10" },
		func(r *controlv1.DeliveryResult) { r.SmtpEnhanced = "" },
	} {
		other := proto.Clone(&acked).(*controlv1.DeliveryResult)
		change(other)
		if sameResult(sent, other) {
			t.Fatal("changed stage or enhanced code accepted")
		}
	}
}

// Old Edge with a new control plane: no stage or code on the wire, the echo has
// none, and the comparison holds.
func TestOldWorkerResultIsAcknowledgedByNewControl(t *testing.T) {
	old := preS34Result(t)
	fields := old.Descriptor().Fields()
	old.Set(fields.ByName("attempt_id"), protoString("attempt"))
	old.Set(fields.ByName("delivery_id"), protoString("delivery"))
	wire, err := proto.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var received controlv1.DeliveryResult
	if err = proto.Unmarshal(wire, &received); err != nil {
		t.Fatal(err)
	}
	if received.SmtpStage != controlv1.SmtpStage_SMTP_STAGE_UNSPECIFIED || received.SmtpEnhanced != "" {
		t.Fatal("old result decoded with a stage")
	}
	echo, _ := proto.Marshal(&received)
	var acked controlv1.DeliveryResult
	if err = proto.Unmarshal(echo, &acked); err != nil || !sameResult(&received, &acked) {
		t.Fatal("echo mismatch", err)
	}
}

// A result replayed from the journal carries the stage and code it was stored
// with, and the acknowledgement it produces deletes the journal entry.
func TestJournalReplayCarriesStageAndAcknowledges(t *testing.T) {
	attempt := journal.Attempt{ID: "attempt", DeliveryID: "delivery", Epoch: 3, Outcome: journal.Rejected, SMTPCode: 550, SMTPStage: "RCPT", SMTPEnhanced: "5.1.1"}
	result, err := toResult(attempt)
	if err != nil || !sameResult(result, newResult()) {
		t.Fatalf("replayed result %v %v", result, err)
	}
	legacy, err := toResult(journal.Attempt{ID: "attempt", DeliveryID: "delivery", Epoch: 3, Outcome: journal.Rejected, SMTPCode: 550})
	if err != nil || legacy.SmtpStage != controlv1.SmtpStage_SMTP_STAGE_UNSPECIFIED || legacy.SmtpEnhanced != "" {
		t.Fatalf("legacy journal entry replayed with a stage: %v %v", legacy, err)
	}
}

func protoString(s string) protoreflect.Value { return protoreflect.ValueOfString(s) }
