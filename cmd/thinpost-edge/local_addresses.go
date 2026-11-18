package main

import (
	"errors"
	"net"
	"net/netip"
)

// requireLocalAddresses refuses to start unless every configured outbound
// source is assigned to a local interface. Without this a missing Multus
// attachment would surface only as failed deliveries; a worker must never
// advertise an address it cannot bind. The check names no address.
func requireLocalAddresses(sources []netip.Addr, interfaceAddrs func() ([]net.Addr, error)) error {
	addrs, err := interfaceAddrs()
	if err != nil {
		return errors.New("cannot list local interface addresses")
	}
	local := make(map[netip.Addr]struct{}, len(addrs))
	for _, item := range addrs {
		var ip net.IP
		switch value := item.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		if address, ok := netip.AddrFromSlice(ip); ok {
			local[address.Unmap()] = struct{}{}
		}
	}
	for _, source := range sources {
		if _, ok := local[source]; !ok {
			return errors.New("a configured SMTP source address is not assigned to this host")
		}
	}
	return nil
}
