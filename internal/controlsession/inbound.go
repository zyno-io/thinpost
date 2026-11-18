package controlsession

import (
	"context"
	"crypto/sha256"
	"errors"
	"sort"
	"strconv"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/inbound"
)

// Inbound is the trusted intake for inbound mail. Receive must return
// inbound.Accepted only after the message is durably committed.
type Inbound interface {
	Receive(context.Context, inbound.Message) inbound.Verdict
	Routes(context.Context) ([]inbound.Route, error)
}

// DefaultRoutesInterval is how often the routing snapshot is rebuilt; it is
// pushed only when it changed.
const DefaultRoutesInterval = 10 * time.Second

func inboundMessage(start *controlv1.InboundStart, body []byte) inbound.Message {
	return inbound.Message{
		ID: start.InboundId, MailFrom: start.MailFrom, Rcpts: start.RcptTo, ClientIP: start.ClientIp, Helo: start.Helo,
		TLS: inbound.TLSInfo{Version: start.TlsVersion, Cipher: start.TlsCipher}, Receiver: start.Receiver,
		Received: time.UnixMilli(start.ReceivedUnixMs), Group: start.Group, Body: body,
	}
}

func inboundVerdict(v inbound.Verdict) controlv1.InboundVerdict {
	switch v {
	case inbound.Accepted:
		return controlv1.InboundVerdict_INBOUND_VERDICT_ACCEPTED
	case inbound.Rejected:
		return controlv1.InboundVerdict_INBOUND_VERDICT_REJECTED
	}
	return controlv1.InboundVerdict_INBOUND_VERDICT_TEMPFAIL
}

// routePages numbers the apps (a group is stable only within one snapshot),
// hashes the snapshot to detect change, and cuts it into pages that fit a
// control frame. Routes are sorted by domain.
func routePages(routes []inbound.Route, tempfailAll bool) ([]*controlv1.ControlFrame, [32]byte, error) {
	if len(routes) > edgeprotocol.MaxRoutes {
		return nil, [32]byte{}, errors.New("too many inbound routes")
	}
	sorted := append([]inbound.Route(nil), routes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Domain < sorted[j].Domain })
	apps := make([]string, 0, 8)
	seen := make(map[string]struct{})
	for _, route := range sorted {
		if _, ok := seen[route.AppID]; !ok {
			seen[route.AppID] = struct{}{}
			apps = append(apps, route.AppID)
		}
	}
	sort.Strings(apps)
	group := make(map[string]uint32, len(apps))
	for i, app := range apps {
		group[app] = uint32(i + 1)
	}
	digest := sha256.New()
	var pages []*controlv1.ControlFrame
	page := &controlv1.InboundRoutes{}
	size, offset := 0, 0
	flush := func(last bool) {
		page.Offset, page.Last = uint32(offset), last
		pages = append(pages, &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Routes{Routes: page}})
		offset += len(page.Routes)
		page, size = &controlv1.InboundRoutes{}, 0
	}
	for _, route := range sorted {
		state := controlv1.InboundRouteState_INBOUND_ROUTE_STATE_TEMPFAIL
		if route.Accept && !tempfailAll {
			state = controlv1.InboundRouteState_INBOUND_ROUTE_STATE_ACCEPT
		}
		entry := &controlv1.InboundRoute{Domain: route.Domain, Group: group[route.AppID], State: state}
		if !edgeprotocol.ValidRoute(entry) {
			// A name the Edge would refuse must not poison the snapshot.
			continue
		}
		cost := edgeprotocol.RoutePageSize(entry)
		if len(page.Routes) > 0 && size+cost > edgeprotocol.MaxRoutePageBytes {
			flush(false)
		}
		page.Routes = append(page.Routes, entry)
		size += cost
		_, _ = digest.Write([]byte(entry.Domain + "\x00" + route.AppID + "\x00" + strconv.Itoa(int(state)) + "\n"))
	}
	flush(true)
	var sum [32]byte
	copy(sum[:], digest.Sum(nil))
	return pages, sum, nil
}
