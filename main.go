package main

import (
	"context"
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
	// InterfaceNamePrefix is the prefix for the interfaces created by Divisor
	InterfaceNamePrefix = "divisor"
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
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	redisClient := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: os.Getenv("REDIS_PASSWORD"),
	})

	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return redisClient, nil
}

func handleNetworkReconfiguration() error {
	numIPs, err := strconv.Atoi(os.Getenv("NUM_IPS"))
	if err != nil {
		return fmt.Errorf("failed to convert NUM_IPS to int: %w", err)
	}

	// Configure divisor interface with the chosen IP
	addresses, err := configureDivisorInterface(numIPs)
	if err != nil {
		return fmt.Errorf("failed to configure divisor interface: %w", err)
	}

	// Configure NAT rules for the selected IP
	targetSubnets := strings.Split(os.Getenv("TARGET_SUBNETS"), ",")

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

// configureDivisorInterface configures the divisor interface with the number of IPs specified
func configureDivisorInterface(numIPs int) ([]string, error) {
	iface := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{
			Name: InterfaceNamePrefix,
		},
	}

	// Add the interface if it doesn't exist
	if err := netlink.LinkAdd(iface); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("failed to add link: %w", err)
	}

	link, err := netlink.LinkByName(InterfaceNamePrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to get link %s by name: %w", InterfaceNamePrefix, err)
	}

	// Remove any existing addresses
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses: %w", err)
	}
	for _, addr := range addrs {
		if err := netlink.AddrDel(link, &addr); err != nil {
			return nil, fmt.Errorf("failed to delete address: %w", err)
		}
	}

	var addresses []string
	// Find and assign new IP addresses
	for i := 0; i < numIPs; i++ {
		ipAddr, err := getUnusedAddress(os.Getenv("DESIRED_SUBNET"))
		if err != nil {
			return nil, fmt.Errorf("failed to get unused address: %w", err)
		}
		slog.Debug("Chosen IP for all runners", "ip", ipAddr)

		// Assign the new IP address
		subnet := strings.Split(os.Getenv("DESIRED_SUBNET"), "/")[1]
		mask, err := strconv.Atoi(subnet)
		if err != nil {
			return nil, fmt.Errorf("failed to convert subnet to int: %w", err)
		}
		newIP := &net.IPNet{
			IP:   net.ParseIP(ipAddr),
			Mask: net.CIDRMask(mask, 32),
		}

		if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: newIP}); err != nil {
			return nil, fmt.Errorf("failed to add address: %w", err)
		}

		slog.Info("Configured divisor interface", "ip", ipAddr)
		addresses = append(addresses, ipAddr)
	}

	// Bring the interface up
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("failed to set link up: %w", err)
	}

	return addresses, nil
}

func configureNATRules(addresses []string, dockerAddresses, targetSubnets []string) error {
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

	for _, dockerIP := range dockerAddresses {
		for _, subnet := range targetSubnets {
			ip := addresses[rng.Intn(len(addresses))]
			err := table.ProgramRule(iptables.Nat, "POSTROUTING", iptables.Insert, []string{"-s", dockerIP, "-d", subnet, "-j", "SNAT", "--to-source", ip, "--random-fully"})
			if err != nil {
				return fmt.Errorf("failed to add SNAT rule for Docker IP %s and subnet %s: %w", dockerIP, subnet, err)
			}
			slog.Debug("Added SNAT rule", "dockerIP", dockerIP, "subnet", subnet, "ip", ip)
		}
	}

	slog.Info("Configured NAT rules", "num_rules", len(addresses))
	return nil
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

	dockerAddresses := []string{}
	for _, container := range containers {
		if !strings.Contains(container.Names[0], "quotient-runner-") {
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

func getUnusedAddress(desiredSubnet string) (string, error) {
	// calculate an unused address in the desired subnet
	_, desiredNet, err := net.ParseCIDR(desiredSubnet)
	if err != nil {
		return "", fmt.Errorf("failed to parse desired subnet: %w", err)
	}

	// calculate how many addresses are in the subnet
	ones, bits := desiredNet.Mask.Size()
	numAddresses := 1 << uint(bits-ones)

	// randomly pick a number between 0 and numAddresses as n
	// pick the nth address in the subnet

	for {
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		n := rng.Intn(numAddresses - 1)
		address := net.IP(make([]byte, 4))
		copy(address, desiredNet.IP)
		for i := 0; i < n; i++ {
			for j := 3; j >= 0; j-- {
				address[j]++
				if address[j] != 0 {
					break
				}
			}
		}

		// ping the address to see if it's in use
		pinger, err := probing.NewPinger(address.String())
		if err != nil {
			return "", fmt.Errorf("failed to create pinger: %w", err)
		}

		pinger.Count = 1
		pinger.Timeout = 1 * time.Second
		pinger.SetPrivileged(true)
		err = pinger.Run()
		if err != nil {
			return "", fmt.Errorf("failed to run pinger: %w", err)
		}

		if pinger.Statistics().PacketsRecv == 0 {
			return address.String(), nil
		}
	}
}
