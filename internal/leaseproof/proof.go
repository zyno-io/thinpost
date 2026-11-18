// Package leaseproof signs database-issued controller leases using the
// controller's existing mTLS private key and verifies them locally at edge.
package leaseproof

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/controltls"
	"github.com/zyno-io/thinpost/internal/edgesession"
	"github.com/zyno-io/thinpost/internal/journal"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

const purpose = "thinpost-controller-lease-v2\x00"
const maxLease = 5 * time.Minute

type Lease struct {
	WorkerID         string `json:"worker_id"`
	ControllerID     string `json:"controller_id"`
	JournalID        string `json:"journal_id"`
	Identity         string `json:"identity"`
	Epoch            uint64 `json:"epoch"`
	ExpiresUnixMilli int64  `json:"expires_unix_milli"`
}

// Sign must only be called on a currently valid database-issued lease. The
// worker trusts authenticated control-plane processes to enforce this rule.
func Sign(lease Lease, signer crypto.Signer) ([]byte, []byte, error) {
	if !validLease(lease) || signer == nil {
		return nil, nil, errors.New("invalid lease")
	}
	payload, err := json.Marshal(lease)
	if err != nil {
		return nil, nil, err
	}
	message := append([]byte(purpose), payload...)
	var signature []byte
	switch key := signer.Public().(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 {
			return nil, nil, errors.New("weak lease signing key")
		}
		digest := sha256.Sum256(message)
		signature, err = signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	case *ecdsa.PublicKey:
		digest := sha256.Sum256(message)
		signature, err = signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	case ed25519.PublicKey:
		signature, err = signer.Sign(rand.Reader, message, crypto.Hash(0))
	default:
		return nil, nil, errors.New("unsupported lease signing key")
	}
	if err != nil {
		return nil, nil, err
	}
	return payload, signature, nil
}

type Authority struct {
	skew time.Duration
	now  func() time.Time
}

// NewAuthority uses a conservative clock-skew margin. Deployment must monitor
// clock synchronization and keep actual skew within this configured bound.
func NewAuthority(skew time.Duration) (*Authority, error) {
	if skew <= 0 || skew > 30*time.Second {
		return nil, errors.New("invalid clock skew bound")
	}
	return &Authority{skew: skew, now: time.Now}, nil
}

type grant struct {
	deadline time.Time
	now      func() time.Time
}

func (g grant) Valid() bool         { return g.now().Before(g.deadline) }
func (g grant) Deadline() time.Time { return g.deadline }

// Authorize implements the edge session Authority without any edge-to-trusted
// network connection. The signature must match the verified mTLS peer's key.
func (a *Authority) Authorize(ctx context.Context, identity string, open *controlv1.Open) (edgesession.Grant, error) {
	if a == nil || a.now == nil || open == nil || len(open.LeasePayload) == 0 || len(open.LeasePayload) > 2048 || len(open.LeaseSignature) == 0 || len(open.LeaseSignature) > 1024 {
		return nil, errors.New("invalid lease proof")
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, errors.New("missing peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil, errors.New("missing TLS peer")
	}
	verified, err := controltls.Identity(info.State, []string{identity})
	if err != nil || verified != identity {
		return nil, errors.New("invalid peer identity")
	}
	var lease Lease
	decoder := json.NewDecoder(bytes.NewReader(open.LeasePayload))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&lease); err != nil {
		return nil, errors.New("invalid lease proof")
	}
	if err = decoder.Decode(new(any)); err != io.EOF || !validLease(lease) || lease.WorkerID != open.WorkerId || lease.ControllerID != open.ControllerId || lease.JournalID != open.JournalId || lease.Epoch != open.Epoch || lease.Identity != identity {
		return nil, errors.New("lease does not match session")
	}
	now := a.now()
	expiry := time.UnixMilli(lease.ExpiresUnixMilli)
	if !now.Add(a.skew).Before(expiry) || expiry.After(now.Add(maxLease+a.skew)) {
		return nil, errors.New("lease is expired or too long")
	}
	certificateExpiry, err := controltls.VerifiedCertificateExpiry(info.State, now, a.skew)
	if err != nil {
		return nil, err
	}
	message := append([]byte(purpose), open.LeasePayload...)
	digest := sha256.Sum256(message)
	switch key := info.State.PeerCertificates[0].PublicKey.(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 || rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], open.LeaseSignature) != nil {
			return nil, errors.New("invalid lease signature")
		}
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(key, digest[:], open.LeaseSignature) {
			return nil, errors.New("invalid lease signature")
		}
	case ed25519.PublicKey:
		if !ed25519.Verify(key, message, open.LeaseSignature) {
			return nil, errors.New("invalid lease signature")
		}
	default:
		return nil, errors.New("unsupported lease signing key")
	}
	if certificateExpiry.Before(expiry) {
		expiry = certificateExpiry
	}
	// Derive a local monotonic deadline where available so later wall-clock
	// adjustments cannot extend an already accepted grant.
	return grant{deadline: now.Add(expiry.Sub(now) - a.skew), now: a.now}, nil
}

func validLease(l Lease) bool {
	return opaque(l.WorkerID) && opaque(l.ControllerID) && journal.ValidIdentity(l.JournalID) && l.Identity != "" && len(l.Identity) <= 512 && l.Epoch > 0 && l.ExpiresUnixMilli > 0
}
func opaque(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
