package acceptance

import (
	"crypto"
	"crypto/rsa"
	"io"
	"math/big"
	"net/mail"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/message"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/tracking"
)

// This public, fixed sizing key never encrypts a delivered link. It measures
// the same self-contained encrypted link format without access to runtime keys.
var preflightTrackingKeys = tracking.NewSizingLinkKeys()

// Preflight signs no real mail. It measures an actual MIME rendering using a
// maximum-size supported RSA signature, selector and recipient header.
// Tracking is rendered with a maximum-length placeholder origin. That is an
// upper bound on any real rendering: a configured origin is validated by
// tracking.NormalizeOrigin to ASCII letters, digits, '-', '.' and a numeric
// port, at most MaxOriginBytes, so it needs no HTML or quoted-printable
// escaping, and a link payload has a fixed size per destination.
// The independent preparation limit remains authoritative for stored messages
// produced by older versions and for future implementation changes.
func preflight(m submission.Message, listUnsubscribe bool) error {
	prepared := m
	if listUnsubscribe {
		// Preparation adds a signed one-click header per delivery. Measure the
		// longest one: the longest permitted origin; the v2 payload is fixed-size for a delivery ID.
		origin := "https://" + strings.Repeat("a", tracking.MaxOriginBytes-len("https://"))
		link, err := preflightTrackingKeys.UnsubscribeLink(origin, strings.Repeat("a", 32))
		if err != nil {
			return submission.ErrInvalidMessage
		}
		prepared.ListUnsubscribe, prepared.ListUnsubscribePost = "<"+link+">", true
	}
	if m.Tracking != nil {
		origin := "https://" + strings.Repeat("a", tracking.MaxOriginBytes-len("https://"))
		html, mappings, err := tracking.Rewrite(m.HTML, tracking.Options{Origin: origin, Links: preflightTrackingKeys, Opens: m.Tracking.Opens, Clicks: m.Tracking.Clicks})
		if err != nil {
			return submission.ErrInvalidMessage
		}
		// Preparation retains the original HTML if no tracking mapping was
		// created, so measure that same representation in this case.
		if len(mappings) > 0 {
			prepared.HTML = html
		}
	}
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return submission.ErrInvalidMessage
	}
	at := strings.LastIndexByte(from.Address, '@')
	if at < 1 {
		return submission.ErrInvalidMessage
	}
	// The To and Cc headers list every recipient on every copy, so the size does
	// not depend on which recipient is rendered.
	for _, raw := range m.Envelope() {
		if _, err := mail.ParseAddress(raw); err != nil {
			return submission.ErrInvalidMessage
		}
	}
	final, err := message.Build(prepared, message.Options{
		Recipient: m.To[0], ListRecipients: true, ID: strings.Repeat("a", 32), Date: time.Unix(0, 0).UTC(),
		SigningDomain: strings.ToLower(from.Address[at+1:]), Selector: strings.Repeat("a", 63), Signer: sizingSigner{},
	})
	if err != nil || len(final)+4096 > edgeprotocol.MaxMIMEBytes {
		return submission.ErrInvalidMessage
	}
	return nil
}

type sizingSigner struct{}

func (sizingSigner) Public() crypto.PublicKey {
	return &rsa.PublicKey{N: new(big.Int).Lsh(big.NewInt(1), message.MaxDKIMRSABits-1), E: 65537}
}

func (sizingSigner) Sign(_ io.Reader, _ []byte, _ crypto.SignerOpts) ([]byte, error) {
	// A fixed-width stand-in, never a usable signature.
	return make([]byte, message.MaxDKIMRSABits/8), nil
}

var _ crypto.Signer = sizingSigner{}
