// Package journal persists bounded, metadata-only edge SMTP attempt records.
// It is not a mail queue and must never contain message bodies or recipients.
package journal

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	ErrFull              = errors.New("attempt journal is full")
	ErrExists            = errors.New("attempt or delivery already in journal")
	ErrNotFound          = errors.New("attempt not found")
	ErrInvalidTransition = errors.New("invalid attempt transition")
	ErrStaleController   = errors.New("controller is not the active fenced owner")
	ErrUnidentified      = errors.New("existing journal has no identity; recovery is required")
	ErrUnavailable       = errors.New("attempt journal requires recovery")
)

const (
	InFlight              = "in_flight"
	Accepted              = "accepted"
	Rejected              = "rejected"
	RouteRejected         = "route_rejected"
	RecipientPolicyDenied = "recipient_policy_denied"
	Uncertain             = "uncertain"
)

var attemptsBucket = []byte("attempts")
var deliveriesBucket = []byte("deliveries")
var controlBucket = []byte("control")
var fenceKey = []byte("fence")
var identityKey = []byte("identity")

type Fence struct {
	ControllerID string `json:"controller_id"`
	Epoch        uint64 `json:"epoch"`
}

type Attempt struct {
	ID         string    `json:"id"`
	DeliveryID string    `json:"delivery_id"`
	Epoch      uint64    `json:"epoch"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Outcome    string    `json:"outcome"`
	SMTPCode   int       `json:"smtp_code,omitempty"`
	// Added with schema 34. Absent in journals written by older workers, which
	// replay with both empty; older workers ignore them in newer journals.
	SMTPStage    string `json:"smtp_stage,omitempty"`
	SMTPEnhanced string `json:"smtp_enhanced,omitempty"`
}

type Journal struct {
	db       *bolt.DB
	capacity int
	reserve  storageReserve
	mu       sync.Mutex
	faulted  bool
	closed   bool
}

// operation serializes database access so no new operation can pass the fault
// check while another operation is discovering a storage or corruption error.
// A process restart is required to reopen and recover the journal. Expected
// state-machine rejections do not indicate a storage failure.
func (j *Journal) operation(fn func() error, expected ...error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.faulted {
		return ErrUnavailable
	}
	err := fn()
	if err != nil {
		for _, allowed := range expected {
			if errors.Is(err, allowed) {
				return err
			}
		}
		j.faulted = true
	}
	return err
}

func (j *Journal) view(fn func(*bolt.Tx) error) error {
	return j.operation(func() error { return j.db.View(fn) })
}

func (j *Journal) update(fn func(*bolt.Tx) error, expected ...error) error {
	return j.operation(func() error { return j.db.Update(fn) }, expected...)
}

// Open obtains an exclusive process lock. The worker must use a persistent,
// access-controlled volume with enough reserved space for result updates.
// A storage error requires the worker to stop claiming work; count bounds do
// not by themselves guarantee free disk space or bound the physical file size.
func Open(path string, capacity int) (*Journal, error) {
	if capacity <= 0 {
		return nil, errors.New("journal capacity must be positive")
	}
	info, statErr := os.Lstat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if statErr == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("journal must be a private regular file")
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second, OpenFile: openJournalFile})
	if err != nil {
		return nil, err
	}
	if err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(attemptsBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(deliveriesBucket); err != nil {
			return err
		}
		control, err := tx.CreateBucketIfNotExists(controlBucket)
		if err != nil {
			return err
		}
		identity := control.Get(identityKey)
		if identity != nil {
			if !ValidIdentity(string(identity)) {
				return errors.New("invalid journal identity")
			}
			return nil
		}
		// Never invent an identity for a journal that already contains evidence.
		// A legacy journal must be handled by an explicit recovery procedure.
		if control.Stats().KeyN != 0 || tx.Bucket(attemptsBucket).Stats().KeyN != 0 || tx.Bucket(deliveriesBucket).Stats().KeyN != 0 {
			return ErrUnidentified
		}
		var raw [16]byte
		if _, err = rand.Read(raw[:]); err != nil {
			return err
		}
		return control.Put(identityKey, []byte(hex.EncodeToString(raw[:])))
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &Journal{db: db, capacity: capacity}, nil
}

// OpenWithReserve additionally preallocates private filesystem space for
// outcome updates. Only use a dedicated persistent filesystem, and monitor
// both filesystem and backing-store capacity independently.
func OpenWithReserve(path string, capacity int, reserveBytes int64) (*Journal, error) {
	if reserveBytes <= 0 {
		return nil, errors.New("reserve size must be positive")
	}
	j, err := Open(path, capacity)
	if err != nil {
		return nil, err
	}
	r, err := openStorageReserve(path+".reserve", reserveBytes)
	if err != nil {
		_ = j.Close()
		return nil, err
	}
	j.reserve = r
	return j, nil
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	j.faulted = true
	var reserveErr error
	if j.reserve != nil {
		reserveErr = j.reserve.close()
	}
	return errors.Join(reserveErr, j.db.Close())
}

// Identity returns the stable identifier committed with this journal. A new
// volume receives a different identity and must not silently replace this one.
func (j *Journal) Identity() (string, error) {
	var identity string
	err := j.view(func(tx *bolt.Tx) error {
		identity = string(tx.Bucket(controlBucket).Get(identityKey))
		if !ValidIdentity(identity) {
			return errors.New("invalid journal identity")
		}
		return nil
	})
	return identity, err
}

func ValidIdentity(value string) bool {
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

// Activate persists a monotonically increasing controller epoch. The caller
// must authenticate the controller and verify that the authority granted this
// epoch before calling Activate. An equal epoch may only renew the same owner.
// Previously started attempts remain in the journal for reconciliation.
func (j *Journal) Activate(fence Fence) error {
	if !validID(fence.ControllerID) || fence.Epoch == 0 {
		return ErrInvalidTransition
	}
	return j.update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(controlBucket)
		raw := bucket.Get(fenceKey)
		if raw != nil {
			var current Fence
			if err := json.Unmarshal(raw, &current); err != nil {
				return err
			}
			if fence.Epoch < current.Epoch || fence.Epoch == current.Epoch && fence.ControllerID != current.ControllerID {
				return ErrStaleController
			}
		}
		value, err := json.Marshal(fence)
		if err != nil {
			return err
		}
		return bucket.Put(fenceKey, value)
	}, ErrStaleController)
}

// IsCurrent checks the persisted fence before starting external SMTP work.
// A storage failure must be treated as loss of authorization.
func (j *Journal) IsCurrent(fence Fence) (bool, error) {
	current := false
	err := j.view(func(tx *bolt.Tx) error {
		raw := tx.Bucket(controlBucket).Get(fenceKey)
		if raw == nil {
			return nil
		}
		var stored Fence
		if err := json.Unmarshal(raw, &stored); err != nil {
			return err
		}
		current = stored == fence
		return nil
	})
	return current, err
}

// Begin must commit before an SMTP connection is attempted. A delivery may
// have only one unacknowledged attempt in this journal, including uncertain
// results. Attempt IDs and delivery IDs must be opaque, non-sensitive tokens.
func (j *Journal) Begin(id, deliveryID string, fence Fence, now time.Time) error {
	if !validID(id) || !validID(deliveryID) || !validID(fence.ControllerID) || fence.Epoch == 0 || now.IsZero() {
		return ErrInvalidTransition
	}
	return j.update(func(tx *bolt.Tx) error {
		if err := j.ensureReserve(); err != nil {
			return err
		}
		raw := tx.Bucket(controlBucket).Get(fenceKey)
		if raw == nil {
			return ErrStaleController
		}
		var current Fence
		if err := json.Unmarshal(raw, &current); err != nil {
			return err
		}
		if current != fence {
			return ErrStaleController
		}
		a := tx.Bucket(attemptsBucket)
		d := tx.Bucket(deliveriesBucket)
		if a.Get([]byte(id)) != nil || d.Get([]byte(deliveryID)) != nil {
			return ErrExists
		}
		if a.Stats().KeyN >= j.capacity {
			return ErrFull
		}
		data, err := json.Marshal(Attempt{ID: id, DeliveryID: deliveryID, Epoch: fence.Epoch, StartedAt: now.UTC(), Outcome: InFlight})
		if err != nil {
			return err
		}
		if err := a.Put([]byte(id), data); err != nil {
			return err
		}
		return d.Put([]byte(deliveryID), []byte(id))
	}, ErrStaleController, ErrExists, ErrFull)
}

// Complete records an outcome durably before it may be reported as durable.
func (j *Journal) Complete(id, outcome string, smtpCode int, now time.Time) error {
	return j.CompleteReply(id, outcome, smtpCode, "", "", now)
}

// CompleteReply is Complete plus the SMTP command stage (MAIL, RCPT or DATA)
// and enhanced status code of the reply. Both may be empty; neither is ever
// reply text. A stage or enhanced code requires an SMTP reply code.
func (j *Journal) CompleteReply(id, outcome string, smtpCode int, stage, enhanced string, now time.Time) error {
	if (stage != "" && stage != "MAIL" && stage != "RCPT" && stage != "DATA") || (stage != "" || enhanced != "") && (smtpCode < 400 || smtpCode > 599) || enhanced != "" && !ValidEnhanced(enhanced) {
		return ErrInvalidTransition
	}
	if now.IsZero() || !(outcome == Accepted && smtpCode == 250 || outcome == Rejected && (smtpCode == 0 || smtpCode >= 400 && smtpCode <= 599) || outcome == RouteRejected && smtpCode >= 400 && smtpCode <= 599 || (outcome == Uncertain || outcome == RecipientPolicyDenied) && smtpCode == 0) {
		return ErrInvalidTransition
	}
	return j.outcomeUpdate(func() error {
		return j.db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(attemptsBucket)
			raw := bucket.Get([]byte(id))
			if raw == nil {
				return ErrNotFound
			}
			var attempt Attempt
			if err := json.Unmarshal(raw, &attempt); err != nil {
				return err
			}
			if attempt.Outcome != InFlight {
				return ErrInvalidTransition
			}
			attempt.Outcome = outcome
			attempt.SMTPCode = smtpCode
			attempt.SMTPStage = stage
			attempt.SMTPEnhanced = enhanced
			attempt.FinishedAt = now.UTC()
			data, err := json.Marshal(attempt)
			if err != nil {
				return err
			}
			return bucket.Put([]byte(id), data)
		})
	})
}

// Recover marks attempts left in flight on worker restart as uncertain. Call
// before accepting work, when no previous owner can still send on this IP.
func (j *Journal) Recover(now time.Time) error {
	if now.IsZero() {
		return ErrInvalidTransition
	}
	return j.outcomeUpdate(func() error {
		return j.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(attemptsBucket)
			var pending []Attempt
			if err := b.ForEach(func(_, raw []byte) error {
				var attempt Attempt
				if err := json.Unmarshal(raw, &attempt); err != nil {
					return err
				}
				if attempt.Outcome != InFlight {
					return nil
				}
				pending = append(pending, attempt)
				return nil
			}); err != nil {
				return err
			}
			for _, attempt := range pending {
				attempt.Outcome = Uncertain
				attempt.FinishedAt = now.UTC()
				data, err := json.Marshal(attempt)
				if err != nil {
					return err
				}
				if err := b.Put([]byte(attempt.ID), data); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

func (j *Journal) List() ([]Attempt, error) {
	var result []Attempt
	err := j.view(func(tx *bolt.Tx) error {
		return tx.Bucket(attemptsBucket).ForEach(func(_, raw []byte) error {
			var attempt Attempt
			if err := json.Unmarshal(raw, &attempt); err != nil {
				return err
			}
			result = append(result, attempt)
			return nil
		})
	})
	return result, err
}

func (j *Journal) Remaining() (int, error) {
	remaining := 0
	err := j.operation(func() error {
		if err := j.ensureReserve(); err != nil {
			if errors.Is(err, ErrFull) {
				return nil
			}
			return err
		}
		return j.db.View(func(tx *bolt.Tx) error {
			remaining = j.capacity - tx.Bucket(attemptsBucket).Stats().KeyN
			if remaining < 0 {
				remaining = 0
			}
			return nil
		})
	})
	return remaining, err
}

// Acknowledge is called only after the authenticated control plane confirms a
// durable commit of this exact result. In-flight attempts cannot be deleted.
func (j *Journal) Acknowledge(expected Attempt) error {
	return j.update(func(tx *bolt.Tx) error {
		a := tx.Bucket(attemptsBucket)
		raw := a.Get([]byte(expected.ID))
		if raw == nil {
			return ErrNotFound
		}
		var attempt Attempt
		if err := json.Unmarshal(raw, &attempt); err != nil {
			return err
		}
		if attempt.Outcome != Accepted && attempt.Outcome != Rejected && attempt.Outcome != RouteRejected && attempt.Outcome != Uncertain && attempt.Outcome != RecipientPolicyDenied {
			return ErrInvalidTransition
		}
		if attempt.DeliveryID != expected.DeliveryID || attempt.Epoch != expected.Epoch || attempt.Outcome != expected.Outcome || attempt.SMTPCode != expected.SMTPCode || attempt.SMTPStage != expected.SMTPStage || attempt.SMTPEnhanced != expected.SMTPEnhanced {
			return ErrInvalidTransition
		}
		if err := tx.Bucket(deliveriesBucket).Delete([]byte(attempt.DeliveryID)); err != nil {
			return err
		}
		return a.Delete([]byte(expected.ID))
	}, ErrNotFound, ErrInvalidTransition)
}

// ValidEnhanced reports whether value is an RFC 3463 code, digits only.
func ValidEnhanced(value string) bool {
	return enhancedPattern.MatchString(value)
}

var enhancedPattern = regexp.MustCompile(`^[245]\.\d{1,3}\.\d{1,3}$`)

func validID(id string) bool {
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
