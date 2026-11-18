package format

import "io"

// InboundView is what the console shows of a received message: the same bounded
// parse the webhook payload is built from (headers and the first text and HTML
// parts, at most MaxInboundBody each), never a second MIME reader.
type InboundView struct {
	From, To, Cc, ReplyTo []string // "Name <address>" or the bare address
	Subject               string
	Text, HTML            string
	// Omitted names what could not be shown, as in the webhook payload:
	// "text", "html" or "headers".
	Omitted []string
}

func displayAddresses(list []address) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		if a.Name == "" {
			out = append(out, a.Address)
		} else {
			out = append(out, a.Name+" <"+a.Address+">")
		}
	}
	return out
}

// inboundRender is the receiver name, the forged fields to drop and the fields
// Thinpost prepends: the one derivation Build, ViewInbound and WriteInboundRaw
// share, so the console and the webhook cannot disagree about the bytes.
func inboundRender(m *InboundInput) (receiver string, drop []span, prefix []byte) {
	receiver = m.Receiver
	if receiver == "" {
		receiver = "thinpost.invalid"
	}
	auth := m.authentication()
	return receiver, forgedSpans(m.Raw, receiver), renderPrefix(m, auth, receiver)
}

// ViewInbound parses m.Raw within the same bounds as the webhook renderer.
func ViewInbound(m *InboundInput) InboundView {
	_, drop, prefix := inboundRender(m)
	p := parseInbound(prefix, m.Raw, drop)
	h := p.Headers
	return InboundView{
		From: displayAddresses(h.From), To: displayAddresses(h.To), Cc: displayAddresses(h.Cc), ReplyTo: displayAddresses(h.ReplyTo),
		Subject: h.Subject, Text: p.Text, HTML: p.HTML, Omitted: p.Omitted,
	}
}

// InboundRawSize is the length of the message as WriteInboundRaw writes it.
func InboundRawSize(m *InboundInput) int64 {
	_, drop, prefix := inboundRender(m)
	return int64(len(prefix) + len(m.Raw) - spanBytes(drop))
}

// WriteInboundRaw writes the message exactly as the webhook's raw field carries
// it before base64: Thinpost's Authentication-Results and Received fields, then
// the stored bytes without any field that claims Thinpost's authserv-id. The
// stored bytes are written as slices of m.Raw, so nothing is copied.
func WriteInboundRaw(w io.Writer, m *InboundInput) (int64, error) {
	_, drop, prefix := inboundRender(m)
	var total int64
	n, err := w.Write(prefix)
	total += int64(n)
	if err != nil {
		return total, err
	}
	for _, seg := range keptSegments(m.Raw, drop) {
		n, err = w.Write(seg)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
