// Package objectkey names new uploads so that their eligibility to create a
// database reference can expire independently of S3 request completion.
package objectkey

import (
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	// ReferenceLifetime bounds how long an upload may acquire a new reference.
	ReferenceLifetime = time.Hour
	// FutureSkew tolerates modest differences between control and database clocks.
	FutureSkew = 5 * time.Minute
)

type Key struct {
	Kind, ID string
	Issued   time.Time
}

// New retains the random object ID and includes the issuance time in the S3
// key. A different issuance time is a different key, even for the same ID.
func New(kind, id string, issued time.Time) (string, error) {
	stamp := strconv.FormatInt(issued.Unix(), 10)
	key := kind + "/v1/" + stamp + "/" + id
	if _, err := Parse(key); err != nil {
		return "", err
	}
	return key, nil
}

func Parse(key string) (Key, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 4 || (parts[0] != "submissions" && parts[0] != "final" && parts[0] != "bounces" && parts[0] != "tracking" && parts[0] != "inbound") || parts[1] != "v1" || len(parts[2]) != 10 || len(parts[3]) != 32 || parts[3] != strings.ToLower(parts[3]) {
		return Key{}, errors.New("invalid managed object key")
	}
	seconds, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != parts[2] {
		return Key{}, errors.New("invalid managed object key")
	}
	decoded, err := hex.DecodeString(parts[3])
	if err != nil || len(decoded) != 16 {
		return Key{}, errors.New("invalid managed object key")
	}
	return Key{Kind: parts[0], ID: parts[3], Issued: time.Unix(seconds, 0).UTC()}, nil
}

// ReferencableAt must be checked against the database clock while holding the
// object lock, after storage verification and before publishing a reference.
func (k Key) ReferencableAt(now time.Time) bool {
	return !k.Issued.After(now.Add(FutureSkew)) && now.Before(k.Issued.Add(ReferenceLifetime))
}
