package edgeprotocol

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/netip"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
)

// CapSubmission is the capability both sides advertise before any submission
// frame is sent.
const CapSubmission = "submission/1"

const (
	// MaxSubmissionBytes is the largest decoded message a submission carries.
	MaxSubmissionBytes = 7_864_320
	// MaxSubmissionRecipients equals submission.MaxEnvelopeRecipients.
	MaxSubmissionRecipients = 100
	// MaxSubmissionItems is how many submissions may be assembling or being
	// committed at once, per session.
	MaxSubmissionItems = 16
	// MaxSubmissionClosed bounds the straggler memory of a session.
	MaxSubmissionClosed = 64
	// MaxAuthPending is how many credential checks may be pending per session.
	MaxAuthPending = 16
	// ControlSubmissionBudget is the process-wide byte budget of control's
	// submission lane. EdgeSubmissionBudget is the edge's.
	ControlSubmissionBudget = 64 << 20
	EdgeSubmissionBudget    = 32 << 20
	// SubmissionBudgetOverhead is added to three times the message size when a
	// submission reserves bytes (assembled buffer, object encode, ciphertext).
	SubmissionBudgetOverhead = 192 << 10
	// MaxSubmissionPorts bounds Ready.submission_ports.
	MaxSubmissionPorts = 8

	maxPathBytes = 320
	secretLength = 4 + 43
)

// SubmissionWeight is the number of budget bytes one submission of the given
// decoded size reserves.
func SubmissionWeight(size uint64) int64 { return 3*int64(size) + SubmissionBudgetOverhead }

func isLowerHex(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ValidSubmissionID reports whether value is a 32-character lowercase hex ID.
// It is used for transfer, request, credential and message IDs.
func ValidSubmissionID(value string) bool { return isLowerHex(value) }

// ValidSubmissionSecret reports whether value is "tps_" and 43 base64url
// characters.
func ValidSubmissionSecret(value string) bool {
	if len(value) != secretLength || value[:4] != "tps_" {
		return false
	}
	for _, c := range value[4:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validOptionalIP(value string) bool {
	if value == "" {
		return true
	}
	_, err := netip.ParseAddr(value)
	return err == nil
}

// ValidSubmissionAuth checks one credential check request.
func ValidSubmissionAuth(auth *controlv1.SubmissionAuth) bool {
	return auth != nil && ValidSubmissionID(auth.RequestId) && ValidSubmissionID(auth.CredentialId) && ValidSubmissionSecret(auth.Secret) && validOptionalIP(auth.ClientIp)
}

// ValidSubmissionFraming checks what the chunk protocol itself depends on: the
// ID, the size and the digest. A start that fails it is a protocol violation
// and ends the session; one that only fails ValidSubmissionStart is answered
// with a verdict instead.
func ValidSubmissionFraming(start *controlv1.SubmissionStart) bool {
	return start != nil && ValidSubmissionID(start.SubmissionId) && start.ExpectedSize != 0 && start.ExpectedSize <= MaxSubmissionBytes && len(start.Sha256) == sha256.Size
}

// SubmissionStartVerdict classifies a start that passed framing. It returns
// UNSPECIFIED when the start is acceptable, CREDENTIAL_INVALID when the
// credential cannot be well formed, and MESSAGE_INVALID for an envelope that
// is out of bounds.
func SubmissionStartVerdict(start *controlv1.SubmissionStart) controlv1.SubmissionVerdict {
	if !ValidSubmissionFraming(start) {
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MESSAGE_INVALID
	}
	if !ValidSubmissionID(start.CredentialId) || !ValidSubmissionSecret(start.Secret) {
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CREDENTIAL_INVALID
	}
	if !ValidSubmissionStart(start) {
		return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MESSAGE_INVALID
	}
	return controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNSPECIFIED
}

// ValidSubmissionStart checks the envelope bounds and the credential syntax as
// well as the framing. The envelope's meaning is validated again by control's
// intake.
func ValidSubmissionStart(start *controlv1.SubmissionStart) bool {
	if !ValidSubmissionFraming(start) || !ValidSubmissionID(start.CredentialId) || !ValidSubmissionSecret(start.Secret) {
		return false
	}
	if len(start.RcptTo) == 0 || len(start.RcptTo) > MaxSubmissionRecipients || len(start.TlsVersion) > 64 || !validPath(start.MailFrom) {
		return false
	}
	for _, rcpt := range start.RcptTo {
		if len(rcpt) == 0 || len(rcpt) > maxPathBytes {
			return false
		}
	}
	switch start.Port {
	case 0, 465, 587:
	default:
		return false
	}
	return validOptionalIP(start.ClientIp) && start.ReceivedUnixMs >= 0 && start.ReceivedUnixMs <= 253402300799999
}

func validPath(value string) bool {
	if len(value) > maxPathBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

// ValidSubmissionPorts checks Ready.submission_ports: at most
// MaxSubmissionPorts distinct ports in 1..65535.
func ValidSubmissionPorts(ports []uint32) bool {
	if len(ports) > MaxSubmissionPorts {
		return false
	}
	for i, port := range ports {
		if port < 1 || port > 65535 {
			return false
		}
		for _, other := range ports[:i] {
			if other == port {
				return false
			}
		}
	}
	return true
}

// ChunkAssembler collects one item's ordered chunks and checks its size and
// SHA-256. It allocates nothing until the first chunk arrives, so the caller
// can acquire its resource reservation first. A stream owner must discard it on
// any error.
type ChunkAssembler struct {
	id       string
	size     uint64
	expected [32]byte
	body     []byte
	received uint64
	started  bool
	done     bool
}

func NewChunkAssembler(id string, size uint64, sha []byte) (*ChunkAssembler, error) {
	if id == "" || size == 0 || len(sha) != sha256.Size {
		return nil, errors.New("invalid chunked item")
	}
	a := &ChunkAssembler{id: id, size: size}
	copy(a.expected[:], sha)
	return a, nil
}

// Allocated reports the bytes currently held.
func (a *ChunkAssembler) Allocated() int { return cap(a.body) }

// Add returns the assembled bytes once the final, hash-verified chunk arrives.
func (a *ChunkAssembler) Add(chunk *controlv1.DataChunk) ([]byte, error) {
	if a.done || chunk == nil || chunk.Id != a.id || chunk.Offset != a.received || len(chunk.Data) == 0 || len(chunk.Data) > MaxChunkBytes || uint64(len(chunk.Data)) > a.size-a.received {
		a.fail()
		return nil, errors.New("invalid chunk")
	}
	if !a.started {
		a.body, a.started = make([]byte, 0, int(a.size)), true
	}
	a.body = append(a.body, chunk.Data...)
	a.received += uint64(len(chunk.Data))
	if chunk.Last != (a.received == a.size) {
		a.fail()
		return nil, errors.New("invalid final chunk")
	}
	if !chunk.Last {
		return nil, nil
	}
	a.done = true
	actual := sha256.Sum256(a.body)
	if subtle.ConstantTimeCompare(actual[:], a.expected[:]) != 1 {
		a.body = nil
		return nil, errors.New("digest mismatch")
	}
	result := a.body
	a.body = nil
	return result, nil
}

func (a *ChunkAssembler) fail() { a.done, a.body = true, nil }
