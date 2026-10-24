// Package ippool selects unused addresses from the configured IPv4 CIDRs.
package ippool

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"time"

	probing "github.com/prometheus-community/pro-bing"

	"divisor/internal/config"
)

const maxAddressSearchAttempts = 128

// ParseDesiredSubnets parses a comma-separated list of IPv4 CIDRs.
func ParseDesiredSubnets(raw string) ([]*net.IPNet, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("no desired subnet configured in %s or %s", config.EnvDesiredSubnet, config.EnvDesiredSubnets)
	}

	parts := strings.Split(raw, ",")
	subnets := make([]*net.IPNet, 0, len(parts))
	for _, part := range parts {
		cidr := strings.TrimSpace(part)
		if cidr == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse desired subnet %q: %w", cidr, err)
		}
		ip4 := ipNet.IP.To4()
		if ip4 == nil || len(ipNet.Mask) != net.IPv4len {
			return nil, fmt.Errorf("desired subnet %q is not a valid IPv4 CIDR", cidr)
		}
		ipNet.IP = ip4.Mask(ipNet.Mask)
		subnets = append(subnets, ipNet)
	}

	if len(subnets) == 0 {
		return nil, fmt.Errorf("no desired subnet configured in %s or %s", config.EnvDesiredSubnet, config.EnvDesiredSubnets)
	}
	return subnets, nil
}

func subnetUsableCount(subnet *net.IPNet) int {
	ones, bits := subnet.Mask.Size()
	hostBits := bits - ones
	switch {
	case hostBits < 0 || hostBits > 30:
		return 0
	case hostBits == 0:
		return 1 // /32 single host
	case hostBits == 1:
		return 2 // /31 point-to-point (RFC 3021)
	default:
		return (1 << uint(hostBits)) - 2 // exclude network (.0) and broadcast (.255)
	}
}

// TotalSubnetsCapacity is the number of addresses pickRandomAddress can return.
func TotalSubnetsCapacity(subnets []*net.IPNet) int {
	total := 0
	for _, s := range subnets {
		total += subnetUsableCount(s)
	}
	return total
}

// PickRandomAddress returns one usable address from a uniformly chosen subnet.
func PickRandomAddress(subnets []*net.IPNet, rng *rand.Rand) (*net.IPNet, error) {
	if len(subnets) == 0 {
		return nil, fmt.Errorf("no desired subnets provided")
	}

	desiredNet := subnets[rng.Intn(len(subnets))]
	ones, bits := desiredNet.Mask.Size()
	hostBits := bits - ones
	if hostBits < 0 || hostBits > 30 {
		return nil, fmt.Errorf("unsupported prefix length /%d in subnet %s", ones, desiredNet.String())
	}

	baseIP := binary.BigEndian.Uint32(desiredNet.IP.To4())
	var offset uint32
	switch {
	case hostBits == 0:
		offset = 0
	case hostBits == 1:
		offset = uint32(rng.Intn(2))
	default:
		numUsable := (1 << uint(hostBits)) - 2
		offset = 1 + uint32(rng.Intn(numUsable))
	}

	addrBytes := make(net.IP, net.IPv4len)
	binary.BigEndian.PutUint32(addrBytes, baseIP+offset)

	maskCopy := make(net.IPMask, len(desiredNet.Mask))
	copy(maskCopy, desiredNet.Mask)

	return &net.IPNet{
		IP:   addrBytes,
		Mask: maskCopy,
	}, nil
}

// GetUnusedAddress returns an address that is not in assigned and does not answer ICMP.
func GetUnusedAddress(subnets []*net.IPNet, assigned map[string]bool) (*net.IPNet, error) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for attempt := 0; attempt < maxAddressSearchAttempts; attempt++ {
		candidate, err := PickRandomAddress(subnets, rng)
		if err != nil {
			return nil, err
		}
		ipStr := candidate.IP.String()
		if assigned[ipStr] {
			continue
		}

		// ping the address to see if it's in use
		pinger, err := probing.NewPinger(ipStr)
		if err != nil {
			return nil, fmt.Errorf("failed to create pinger: %w", err)
		}

		pinger.Count = 1
		pinger.Timeout = 1 * time.Second
		pinger.SetPrivileged(true)
		err = pinger.Run()
		if err != nil {
			return nil, fmt.Errorf("failed to run pinger: %w", err)
		}

		if pinger.Statistics().PacketsRecv == 0 {
			return candidate, nil
		}
	}

	return nil, fmt.Errorf("unable to find an unused IP address after %d attempts", maxAddressSearchAttempts)
}
