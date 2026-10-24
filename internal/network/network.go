// Package network rotates addresses on the divisor interface and installs
// either SNAT rules or direct-mode routes.
package network

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/moby/moby/libnetwork/iptables"
	"github.com/vishvananda/netlink"

	"divisor/internal/config"
	"divisor/internal/ippool"
)

// DirectModeReady reports whether promote_secondaries is set for all
// interfaces or for interfaceName. interfaceName is not defaulted: an empty
// name still reads /proc/sys/net/ipv4/conf//promote_secondaries.
func DirectModeReady(interfaceName string) error {
	promoteSet := func(iface string) bool {
		v, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + iface + "/promote_secondaries")
		return err == nil && strings.TrimSpace(string(v)) == "1"
	}
	if !promoteSet("all") && !promoteSet(interfaceName) {
		return fmt.Errorf("net.ipv4.conf.%s.promote_secondaries must be 1 in direct mode", interfaceName)
	}
	return nil
}

// Reconfigure reads the current environment and applies one rotation.
func Reconfigure() error {
	cfg := config.Load()

	numIPs, err := strconv.Atoi(cfg.NumIPs)
	if err != nil {
		return fmt.Errorf("failed to convert NUM_IPS to int: %w", err)
	}

	// Reject a bad target list before the interface is edited. Otherwise a
	// typo rotates addresses and, in SNAT mode, deletes existing NAT rules.
	targetSubnets := strings.Split(cfg.TargetSubnets, ",")
	if cfg.Mode == config.ModeDirect {
		if _, err := parseRouteDestinations(targetSubnets); err != nil {
			return fmt.Errorf("failed to configure routes: %w", err)
		}
	} else if _, err := normalizeSNATSubnets(targetSubnets); err != nil {
		return fmt.Errorf("failed to configure NAT rules: %w", err)
	}

	// Configure divisor interface with the chosen IP
	addresses, err := configureDivisorInterface(cfg, numIPs)
	if err != nil {
		return fmt.Errorf("failed to configure divisor interface: %w", err)
	}

	if cfg.Mode == config.ModeDirect {
		if err := configureRoutes(cfg, targetSubnets); err != nil {
			return fmt.Errorf("failed to configure routes: %w", err)
		}
		slog.Info("Finished reconfiguring network")
		return nil
	}

	// Configure NAT rules for the selected IP
	dockerAddresses, err := getDockerContainerAddresses(cfg)
	if err != nil {
		return fmt.Errorf("failed to get Docker container addresses: %w", err)
	}

	if err := configureNATRules(addresses, dockerAddresses, targetSubnets); err != nil {
		return fmt.Errorf("failed to configure NAT rules: %w", err)
	}

	slog.Info("Finished reconfiguring network")
	return nil
}

// configureRoutes routes the target subnets out the divisor interface.
func configureRoutes(cfg config.Config, targetSubnets []string) error {
	interfaceName := cfg.InterfaceNameOrDefault()
	link, err := netlink.LinkByName(interfaceName)
	if err != nil {
		return fmt.Errorf("failed to get link %s by name: %w", interfaceName, err)
	}

	destinations, err := parseRouteDestinations(targetSubnets)
	if err != nil {
		return err
	}

	gateway := net.ParseIP(cfg.TargetSubnetGateway)
	for _, dst := range destinations {
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst}
		if gateway != nil {
			route.Gw = gateway
			route.Flags = int(netlink.FLAG_ONLINK)
		} else {
			route.Scope = netlink.SCOPE_LINK
		}
		if err := netlink.RouteReplace(route); err != nil {
			return fmt.Errorf("failed to add route to %s: %w", dst, err)
		}
	}
	return nil
}

// configureDivisorInterface configures the divisor interface with the number of IPs specified.
func configureDivisorInterface(cfg config.Config, numIPs int) ([]string, error) {
	if numIPs < 1 {
		return nil, fmt.Errorf("NUM_IPS must be positive, got %d", numIPs)
	}

	// Parse and size the pool before creating or editing the interface.
	// SNAT mode removes current addresses, so a bad NUM_IPS used to wipe
	// them and then fail.
	desiredSubnets, err := ippool.ParseDesiredSubnets(cfg.DesiredSubnets)
	if err != nil {
		return nil, err
	}
	totalCapacity := ippool.TotalSubnetsCapacity(desiredSubnets)
	if numIPs > totalCapacity {
		return nil, fmt.Errorf("requested NUM_IPS (%d) exceeds total usable addresses (%d) in DESIRED_SUBNET", numIPs, totalCapacity)
	}

	interfaceName := cfg.InterfaceNameOrDefault()

	// Check if the interface already exists
	link, err := netlink.LinkByName(interfaceName)
	if err != nil {
		// Interface doesn't exist, create a dummy interface
		iface := &netlink.Dummy{
			LinkAttrs: netlink.LinkAttrs{
				Name: interfaceName,
			},
		}

		if err := netlink.LinkAdd(iface); err != nil {
			return nil, fmt.Errorf("failed to add link: %w", err)
		}

		link, err = netlink.LinkByName(interfaceName)
		if err != nil {
			return nil, fmt.Errorf("failed to get link %s by name: %w", interfaceName, err)
		}
	}

	// Remove any existing addresses
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses: %w", err)
	}
	removeOld := func() error {
		for _, addr := range addrs {
			if err := netlink.AddrDel(link, &addr); err != nil {
				return fmt.Errorf("failed to delete address: %w", err)
			}
		}
		return nil
	}

	// SNAT mode deletes adding to avoid requiring promote_secondaries
	direct := cfg.Mode == config.ModeDirect
	if !direct {
		if err := removeOld(); err != nil {
			return nil, err
		}
	}

	assigned := make(map[string]bool, len(addrs)+numIPs)
	if direct {
		for _, addr := range addrs {
			if addr.IP != nil {
				assigned[addr.IP.String()] = true
			}
		}
	}

	var addresses []string
	// Find and assign new IP addresses
	for i := 0; i < numIPs; i++ {
		newIP, err := ippool.GetUnusedAddress(desiredSubnets, assigned)
		if err != nil {
			return nil, fmt.Errorf("failed to get unused address: %w", err)
		}
		ipAddr := newIP.IP.String()
		assigned[ipAddr] = true
		slog.Debug("Chosen IP for all runners", "ip", ipAddr, "cidr", newIP.String())

		if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: newIP}); err != nil {
			return nil, fmt.Errorf("failed to add address: %w", err)
		}

		slog.Info("Configured divisor interface", "ip", ipAddr, "cidr", newIP.String())
		addresses = append(addresses, ipAddr)
	}

	// Direct mode adds before deleting to avoid temporarily dropping the routes
	if direct {
		if err := removeOld(); err != nil {
			return nil, err
		}
	}

	// Bring the interface up
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("failed to set link up: %w", err)
	}

	return addresses, nil
}

func configureNATRules(addresses []string, dockerAddresses, targetSubnets []string) error {
	normalizedSubnets, err := normalizeSNATSubnets(targetSubnets)
	if err != nil {
		return err
	}
	// Indexing an empty address list panics. Skip the check when there is
	// nothing to install; a round with no runners still clears old SNAT rules.
	if len(normalizedSubnets) > 0 && len(dockerAddresses) > 0 && len(addresses) == 0 {
		return fmt.Errorf("no source addresses available for SNAT")
	}

	// Clear existing SNAT rules
	table := iptables.GetIptable(iptables.IPv4)
	output, err := table.Raw("-t", "nat", "-S", "POSTROUTING")
	if err != nil {
		return fmt.Errorf("failed to list rules: %w", err)
	}
	rules := strings.Split(string(output), "\n")
	for _, rule := range rules {
		if strings.Contains(rule, "-j SNAT") {
			deleteRule := "-t nat " + strings.Replace(rule, "-A", "-D", 1)
			_, err := table.Raw(strings.Fields(deleteRule)...)
			if err != nil {
				slog.Warn("Error deleting rule", "rule", rule, "error", err)
			}
		}
	}

	// Add new SNAT rules for each target subnet and Docker address
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	installed := 0
	for _, dockerIP := range dockerAddresses {
		for _, subnet := range normalizedSubnets {
			ip := addresses[rng.Intn(len(addresses))]
			err := table.ProgramRule(iptables.Nat, "POSTROUTING", iptables.Insert, []string{"-s", dockerIP, "-d", subnet, "-j", "SNAT", "--to-source", ip, "--random-fully"})
			if err != nil {
				return fmt.Errorf("failed to add SNAT rule for Docker IP %s and subnet %s: %w", dockerIP, subnet, err)
			}
			installed++
			slog.Debug("Added SNAT rule", "dockerIP", dockerIP, "subnet", subnet, "ip", ip)
		}
	}

	slog.Info("Configured NAT rules", "num_rules", installed)
	return nil
}

// normalizeSNATSubnets trims entries and drops blanks. An invalid CIDR is an
// error. Direct mode uses parseRouteDestinations, which rejects blanks.
func normalizeSNATSubnets(targetSubnets []string) ([]string, error) {
	normalized := make([]string, 0, len(targetSubnets))
	for _, rawSubnet := range targetSubnets {
		subnet := strings.TrimSpace(rawSubnet)
		if subnet == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(subnet); err != nil {
			return nil, fmt.Errorf("invalid target subnet %q: %w", subnet, err)
		}
		normalized = append(normalized, subnet)
	}
	return normalized, nil
}

// parseRouteDestinations parses every target subnet. A blank entry is an error.
func parseRouteDestinations(targetSubnets []string) ([]*net.IPNet, error) {
	destinations := make([]*net.IPNet, 0, len(targetSubnets))
	for _, subnet := range targetSubnets {
		_, dst, err := net.ParseCIDR(strings.TrimSpace(subnet))
		if err != nil {
			return nil, fmt.Errorf("failed to parse target subnet %q: %w", subnet, err)
		}
		destinations = append(destinations, dst)
	}
	return destinations, nil
}

func getDockerContainerAddresses(cfg config.Config) ([]string, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	containers, err := cli.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list Docker containers: %w", err)
	}

	containerPrefix := cfg.ContainerPrefixOrDefault()

	dockerAddresses := []string{}
	for _, container := range containers {
		if !strings.Contains(container.Names[0], containerPrefix) {
			continue
		}

		containerDetails, err := cli.ContainerInspect(context.Background(), container.ID)
		if err != nil {
			slog.Warn("Failed to inspect container", "containerID", container.ID, "error", err)
			continue
		}
		for _, network := range containerDetails.NetworkSettings.Networks {
			dockerAddresses = append(dockerAddresses, network.IPAddress)
		}
	}

	return dockerAddresses, nil
}
