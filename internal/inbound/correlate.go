package inbound

import (
	"context"
	"strings"

	"github.com/zyno-io/thinpost/internal/messageid"
)

// Candidate is one delivery an identifier from a reply's headers can refer to.
type Candidate struct {
	// Key is the identifier looked up (the Message-ID's hex part).
	Key          string
	MessageID    string
	DeliveryID   string
	SenderDomain string
	// Shared is true when the identifier was a message ID (the message used the
	// shared Message-ID form); false when it was a delivery ID.
	Shared bool
	// Recipient is the delivery's recipient address, lowercased.
	Recipient          string
	EnhancedProtection bool
}

// Correlation links an inbound message to an app's outbound message. It is a
// header claim, not authentication: anyone who saw the outbound mail can quote
// its Message-ID.
type Correlation struct {
	MessageID string
	// DeliveryID is empty for a message-level match (a shared-form Message-ID
	// whose reply could not be attributed to exactly one recipient).
	DeliveryID string
	// Header is "in_reply_to" or "references".
	Header             string
	FromMatches        bool
	EnhancedProtection bool
}

// maxExtract bounds how many bracketed identifiers are read from one header
// before the most recent are chosen; the header is itself cut to 16 KiB.
const maxExtract = 1000

type scanned struct {
	id, domain, header string
}

// scan lists the Thinpost identifiers to try, in order: In-Reply-To first, then
// References from the last entry backwards, at most messageid.MaxScanned
// identifiers examined and each tried once.
func scan(p *Parsed) []scanned {
	var out []scanned
	seen := make(map[string]struct{})
	add := func(values []string, header string) {
		for _, value := range values {
			id, domain, ok := messageid.ParseThinpost(value)
			if !ok {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, scanned{id, domain, header})
		}
	}
	add(messageid.Extract(p.InReplyTo, maxExtract), "in_reply_to")
	refs := messageid.Extract(p.References, maxExtract)
	for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
		refs[i], refs[j] = refs[j], refs[i]
	}
	add(refs, "references")
	if len(out) > messageid.MaxScanned {
		out = out[:messageid.MaxScanned]
	}
	return out
}

// Correlate finds the app's outbound message a reply refers to. Only the app
// that owns the receiving domain is searched, so an identifier from another
// app is skipped silently and reveals nothing. A candidate counts only when
// its recorded sender domain equals the identifier's right-hand side. For the
// legacy form (a delivery ID) the delivery is the match. For the shared form
// (a message ID) the delivery whose recipient equals the reply's From address is
// chosen when exactly one does; otherwise the match is message-level.
func Correlate(ctx context.Context, repo Repository, appID string, p *Parsed) (*Correlation, error) {
	ids := scan(p)
	if len(ids) == 0 {
		return nil, nil
	}
	keys := make([]string, len(ids))
	for i, s := range ids {
		keys[i] = s.id
	}
	candidates, err := repo.ReplyCandidates(ctx, appID, keys)
	if err != nil {
		return nil, err
	}
	for _, s := range ids {
		var matching []Candidate
		for _, c := range candidates {
			if c.Key == s.id && c.SenderDomain != "" && strings.EqualFold(c.SenderDomain, s.domain) {
				matching = append(matching, c)
			}
		}
		if len(matching) == 0 {
			continue
		}
		from := strings.ToLower(p.FromAddress)
		if !matching[0].Shared {
			c := matching[0]
			return &Correlation{MessageID: c.MessageID, DeliveryID: c.DeliveryID, Header: s.header, FromMatches: from != "" && c.Recipient == from, EnhancedProtection: c.EnhancedProtection}, nil
		}
		var picked []Candidate
		for _, c := range matching {
			if from != "" && c.Recipient == from {
				picked = append(picked, c)
			}
		}
		out := &Correlation{MessageID: matching[0].MessageID, Header: s.header, FromMatches: len(picked) > 0, EnhancedProtection: matching[0].EnhancedProtection}
		if len(picked) == 1 {
			out.DeliveryID = picked[0].DeliveryID
		}
		return out, nil
	}
	return nil, nil
}
