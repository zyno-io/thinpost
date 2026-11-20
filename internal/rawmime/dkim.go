package rawmime

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"io"
	"math/big"

	"github.com/emersion/go-msgauth/dkim"
	"golang.org/x/crypto/ed25519"
)

// signedHeaders are the header fields DKIM covers, in the order of the h= list.
var signedHeaders = [...]string{
	"From", "Sender", "Reply-To", "To", "Cc", "Subject", "Date", "Message-ID", "In-Reply-To", "References",
	"MIME-Version", "Content-Type", "Content-Transfer-Encoding", "List-Unsubscribe", "List-Unsubscribe-Post", "List-Id",
}

// SignedHeaderKeys returns the h= list for a prepared header block: each covered
// field name once if present (DKIM selects instances bottom-up, so the
// bottom-most instance is signed), plus one extra From so that adding a second
// From invalidates the signature (oversigning). The list is bounded by the fixed
// name list, so the h= tag stays short however many fields the message has.
func SignedHeaderKeys(head []byte) []string {
	var keys []string
	for _, want := range signedHeaders {
		for pos := 0; pos < len(head); {
			f, _, err := nextField(head, pos)
			if err != nil {
				break
			}
			if equalFoldASCII(head[f.start:f.name], want) {
				keys = append(keys, want)
				break
			}
			pos = f.end
		}
	}
	return append(keys, "From")
}

func signOptions(head []byte, in *PrepareInput, signer crypto.Signer) *dkim.SignOptions {
	return &dkim.SignOptions{
		Domain:                 in.From.Domain,
		Selector:               in.Selector,
		Signer:                 signer,
		Hash:                   crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
		HeaderKeys:             SignedHeaderKeys(head),
	}
}

// sign streams the prepared message into a DKIM signer and returns the
// DKIM-Signature field. message is the header block, CRLF and the body.
func sign(message []byte, hlen int, in *PrepareInput) (string, error) {
	return signWith(message, message[:hlen], in, in.Signer)
}

func signWith(message, head []byte, in *PrepareInput, signer crypto.Signer) (string, error) {
	s, err := dkim.NewSigner(signOptions(head, in, signer))
	if err != nil {
		return "", ErrSigning
	}
	if _, err := s.Write(message); err != nil {
		s.Close()
		return "", ErrSigning
	}
	if err := s.Close(); err != nil {
		return "", ErrSigning
	}
	sig := s.Signature()
	if longestLine([]byte(sig)) > MaxLine {
		return "", ErrInvalid
	}
	return sig, nil
}

// longestLine is the longest CRLF-separated line of b.
func longestLine(b []byte) int {
	longest := 0
	for len(b) > 0 {
		line := b
		if i := bytes.Index(b, crlf); i >= 0 {
			line, b = b[:i], b[i+2:]
		} else {
			b = nil
		}
		longest = max(longest, len(line))
	}
	return longest
}

// signatureBytes is the length of a signature made with the public key.
func signatureBytes(pub crypto.PublicKey) (int, error) {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if k == nil || k.N == nil {
			return 0, ErrInvalid
		}
		return (k.N.BitLen() + 7) / 8, nil
	case ed25519.PublicKey:
		return ed25519.SignatureSize, nil
	}
	return 0, ErrInvalid
}

// signatureSlack bounds the DKIM-Signature field: the fixed tags, d= and s=, an
// h= list of every covered name, and the base64 signature, with
// room for the folding.
func signatureSlack(pub crypto.PublicKey, domain, selector string) (int, error) {
	n, err := signatureBytes(pub)
	if err != nil {
		return 0, err
	}
	b64 := (n + 2) / 3 * 4
	return 1024 + 2*(len(domain)+len(selector)) + (len(signedHeaders)+1)*(len("Content-Transfer-Encoding")+1)*21/20 + b64*21/20, nil
}

// sizingSigner reports a key and signs with zeros of the right width. It never
// makes a usable signature.
type sizingSigner struct {
	pub crypto.PublicKey
	n   int
}

func (s sizingSigner) Public() crypto.PublicKey { return s.pub }

func (s sizingSigner) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return make([]byte, s.n), nil
}

// MaxSizingRSABits is the largest RSA key the sizing signer models. It must
// equal message.MaxDKIMRSABits (checked in internal/message's tests); it is
// repeated here so rawmime does not import internal/message.
const MaxSizingRSABits = 8192

// SizingSigner is a public, unusable RSA signer of the given modulus size for
// PreparedSize: it measures the DKIM header without access to a real key. The
// size must be 1024 to MaxSizingRSABits bits.
func SizingSigner(rsaBits int) (crypto.Signer, error) {
	if rsaBits < 1024 || rsaBits > MaxSizingRSABits {
		return nil, ErrInvalid
	}
	return sizingSigner{pub: &rsa.PublicKey{N: new(big.Int).Lsh(big.NewInt(1), uint(rsaBits-1)), E: 65537}, n: (rsaBits + 7) / 8}, nil
}
