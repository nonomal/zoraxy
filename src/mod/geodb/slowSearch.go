package geodb

import (
	"errors"
	"math/big"
	"net"
)

/*
	slowSearch.go

	This script implement the slow search method for ip to country code
	lookup. If you have the memory allocation for near O(1) lookup,
	you should not be using slow search mode.
*/

func ipv4ToUInt32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func isIPv4InRange(startIP, endIP, testIP string) (bool, error) {
	start := net.ParseIP(startIP)
	end := net.ParseIP(endIP)
	test := net.ParseIP(testIP)

	if start == nil || end == nil || test == nil {
		return false, errors.New("invalid IP address format")
	}

	startUint := ipv4ToUInt32(start)
	endUint := ipv4ToUInt32(end)
	testUint := ipv4ToUInt32(test)

	return testUint >= startUint && testUint <= endUint, nil
}

func isIPv6InRange(startIP, endIP, testIP string) (bool, error) {
	start := net.ParseIP(startIP)
	end := net.ParseIP(endIP)
	test := net.ParseIP(testIP)

	if start == nil || end == nil || test == nil {
		return false, errors.New("invalid IP address format")
	}

	startInt := new(big.Int).SetBytes(start.To16())
	endInt := new(big.Int).SetBytes(end.To16())
	testInt := new(big.Int).SetBytes(test.To16())

	return testInt.Cmp(startInt) >= 0 && testInt.Cmp(endInt) <= 0, nil
}

// Slow country code lookup for
func (s *Store) slowSearchIpv4(ipAddr string) string {
	if isReservedIP(ipAddr) {
		return ""
	}
	for _, ipRange := range s.geodb {
		startIp := ipRange[0]
		endIp := ipRange[1]
		cc := ipRange[2]

		inRange, _ := isIPv4InRange(startIp, endIp, ipAddr)
		if inRange {
			return cc
		}
	}
	return ""
}

func (s *Store) slowSearchIpv6(ipAddr string) string {
	if isReservedIP(ipAddr) {
		return ""
	}
	for _, ipRange := range s.geodbIpv6 {
		startIp := ipRange[0]
		endIp := ipRange[1]
		cc := ipRange[2]

		inRange, _ := isIPv6InRange(startIp, endIp, ipAddr)
		if inRange {
			return cc
		}
	}
	return ""
}
