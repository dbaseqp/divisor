package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/joho/godotenv"
	"github.com/moby/moby/libnetwork/iptables"
	probing "github.com/prometheus-community/pro-bing"
	"github.com/redis/go-redis/v9"
	"github.com/vishvananda/netlink"
)

const (
	// DefaultInterfaceName is the default name for the interface used by Divisor
	DefaultInterfaceName = "divisor"
	// DefaultContainerPrefix is the default prefix for container names to filter
	DefaultContainerPrefix = "quotient-runner"

	// EnvRedisAddr is the environment variable for Redis address
	EnvRedisAddr = "REDIS_ADDR"
	// EnvRedisPassword is the environment variable for Redis password
	EnvRedisPassword = "REDIS_PASSWORD"
	// EnvNumIPs is the environment variable for the number of IPs
	EnvNumIPs = "NUM_IPS"
	// EnvTargetSubnets is the environment variable for target subnets
	EnvTargetSubnets = "TARGET_SUBNETS"
	// EnvInterfaceName is the environment variable for the interface name
	EnvInterfaceName = "INTERFACE_NAME"
	// EnvDesiredSubnet is the environment variable for the desired subnet(s)
	EnvDesiredSubnet = "DESIRED_SUBNET"
	// EnvDesiredSubnets is the plural alias for EnvDesiredSubnet
	EnvDesiredSubnets = "DESIRED_SUBNETS"
	// EnvContainerPrefix is the environment variable for the container prefix
	EnvContainerPrefix = "CONTAINER_PREFIX"
	// EnvMode is the environment variable for the mode of operation
	EnvMode = "DIVISOR_MODE"
	// EnvTargetSubnetGateway is the environment variable for the target subnet gateway
	EnvTargetSubnetGateway = "TARGET_SUBNET_GATEWAY"

	// ModeSNAT is the default mode of operation, SNATting runner containers from the host
	ModeSNAT = "snat"
	// ModeDirect is the mode of operation that sets the IP of a runner directly
	ModeDirect = "direct"
)

var logLvels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

var opts struct {
	logger struct {
		level string
	}
}

func main() {
	// parse command line options
	flag.StringVar(&opts.logger.level, "log-level", "debug", "Set the log level")
	flag.Parse()

	logLevel, ok := logLvels[opts.logger.level]
	if !ok {
		log.Fatalf("Invalid log level: %s", opts.logger.level)
	}
	// use config to setup engine
	var handler slog.Handler
	handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	logger := slog.New(handler)
	slog.SetDefault(logger)

	err := godotenv.Load(".env")
	if err != nil {
		log.Fatalf("Error loading .env file: %s", err)
	}

	if m := os.Getenv(EnvMode); m != "" && m != ModeSNAT && m != ModeDirect {
		log.Fatalf("Invalid %s %q: must be unset, %s or %s", EnvMode, m, ModeSNAT, ModeDirect)
	}

	// In direct mode nothing else configures the interface, so do it on startup
	if os.Getenv(EnvMode) == ModeDirect {
		// New addresses are added before the old ones are removed, so
		// without promote_secondaries we'd lose the new address as well
		promote_set := func(iface string) bool {
			v, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + iface + "/promote_secondaries")
			return err == nil && strings.TrimSpace(string(v)) == "1"
		}
		if !promote_set("all") && !promote_set(os.Getenv(EnvInterfaceName)) {
			log.Fatalf("net.ipv4.conf.%s.promote_secondaries must be 1 in direct mode", os.Getenv(EnvInterfaceName))
		}
		if err := handleNetworkReconfiguration(); err != nil {
			log.Fatalf("Initial network configuration failed: %s", err)
		}
	}

	slog.Info("Starting the Divisor...")
	for {
		// reconnect to redis if needed
		if err := subscribeAndListen(); err != nil {
			slog.Error("main loop errored out", "error", err)
		}
	}
}

func subscribeAndListen() error {
	slog.Debug("Connecting to Redis...")
	redisClient, err := connectToRedis()
	if err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}
	slog.Debug("Connected to Redis")

	events := redisClient.Subscribe(context.Background(), "events")
	defer events.Close()
	eventsChannel := events.Channel()

	for msg := range eventsChannel {
		slog.Info("Received message", "message", msg.String())

		if msg.Payload == "round_finish" {
			slog.Info("round_finish event received, reconfiguring interface")
		} else {
			continue
		}

		if err := handleNetworkReconfiguration(); err != nil {
			slog.Error("Failed to reconfigure network", "error", err)
		}
	}

	return nil
}

func connectToRedis() (*redis.Client, error) {
	redisAddr := os.Getenv(EnvRedisAddr)
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	redisClient := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: os.Getenv(EnvRedisPassword),
	})

	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return redisClient, nil
}

func handleNetworkReconfiguration() error {
	numIPs, err := strconv.Atoi(os.Getenv(EnvNumIPs))
	if err != nil {
		return fmt.Errorf("failed to convert NUM_IPS to int: %w", err)
	}

	// Reject a bad target list before the interface is edited. Otherwise a
	// typo rotates addresses and, in SNAT mode, deletes existing NAT rules.
	targetSubnets := strings.Split(os.Getenv(EnvTargetSubnets), ",")
	if os.Getenv(EnvMode) == ModeDirect {
		if _, err := parseRouteDestinations(targetSubnets); err != nil {
			return fmt.Errorf("failed to configure routes: %w", err)
		}
	} else if _, err := normalizeSNATSubnets(targetSubnets); err != nil {
		return fmt.Errorf("failed to configure NAT rules: %w", err)
	}

	// Configure divisor interface with the chosen IP
	addresses, err := configureDivisorInterface(numIPs)
	if err != nil {
		return fmt.Errorf("failed to configure divisor interface: %w", err)
	}

	if os.Getenv(EnvMode) == ModeDirect {
		if err := configureRoutes(targetSubnets); err != nil {
			return fmt.Errorf("failed to configure routes: %w", err)
		}
		slog.Info("Finished reconfiguring network")
		return nil
	}

	// Configure NAT rules for the selected IP
	dockerAddresses, err := getDockerContainerAddresses()
	if err != nil {
		return fmt.Errorf("failed to get Docker container addresses: %w", err)
	}

	if err := configureNATRules(addresses, dockerAddresses, targetSubnets); err != nil {
		return fmt.Errorf("failed to configure NAT rules: %w", err)
	}

	slog.Info("Finished reconfiguring network")
	return nil
}

// configureRoutes routes the target subnets out the divisor interface
func configureRoutes(targetSubnets []string) error {
	interfaceName := os.Getenv(EnvInterfaceName)
	if interfaceName == "" {
		interfaceName = DefaultInterfaceName
	}
	link, err := netlink.LinkByName(interfaceName)
	if err != nil {
		return fmt.Errorf("failed to get link %s by name: %w", interfaceName, err)
	}

	destinations, err := parseRouteDestinations(targetSubnets)
	if err != nil {
		return err
	}

	gateway := net.ParseIP(os.Getenv(EnvTargetSubnetGateway))
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

// configureDivisorInterface configures the divisor interface with the number of IPs specified
func configureDivisorInterface(numIPs int) ([]string, error) {
	if numIPs < 1 {
		return nil, fmt.Errorf("NUM_IPS must be positive, got %d", numIPs)
	}

	// Parse and size the pool before creating or editing the interface.
	// SNAT mode removes current addresses, so a bad NUM_IPS used to wipe
	// them and then fail.
	desiredSubnets, err := parseDesiredSubnets(getDesiredSubnetsEnv())
	if err != nil {
		return nil, err
	}
	totalCapacity := totalSubnetsCapacity(desiredSubnets)
	if numIPs > totalCapacity {
		return nil, fmt.Errorf("requested NUM_IPS (%d) exceeds total usable addresses (%d) in DESIRED_SUBNET", numIPs, totalCapacity)
	}

	interfaceName := os.Getenv(EnvInterfaceName)
	if interfaceName == "" {
		interfaceName = DefaultInterfaceName
	}

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
	direct := os.Getenv(EnvMode) == ModeDirect
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
		newIP, err := getUnusedAddress(desiredSubnets, assigned)
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

func getDockerContainerAddresses() ([]string, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	containers, err := cli.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list Docker containers: %w", err)
	}

	containerPrefix := os.Getenv(EnvContainerPrefix)
	if containerPrefix == "" {
		containerPrefix = DefaultContainerPrefix
	}

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

func getDesiredSubnetsEnv() string {
	if v := strings.TrimSpace(os.Getenv(EnvDesiredSubnets)); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(EnvDesiredSubnet))
}

func parseDesiredSubnets(raw string) ([]*net.IPNet, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("no desired subnet configured in %s or %s", EnvDesiredSubnet, EnvDesiredSubnets)
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
		return nil, fmt.Errorf("no desired subnet configured in %s or %s", EnvDesiredSubnet, EnvDesiredSubnets)
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

func totalSubnetsCapacity(subnets []*net.IPNet) int {
	total := 0
	for _, s := range subnets {
		total += subnetUsableCount(s)
	}
	return total
}

func pickRandomAddress(subnets []*net.IPNet, rng *rand.Rand) (*net.IPNet, error) {
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

const maxAddressSearchAttempts = 128

func getUnusedAddress(subnets []*net.IPNet, assigned map[string]bool) (*net.IPNet, error) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for attempt := 0; attempt < maxAddressSearchAttempts; attempt++ {
		candidate, err := pickRandomAddress(subnets, rng)
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
