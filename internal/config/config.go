// Package config reads divisor's environment. Mode is checked at startup.
// NUM_IPS and the interface name stay raw: callers parse or default them
// at the same point they did before this package existed.
package config

import (
	"fmt"
	"os"
	"strings"
)

const (
	// DefaultInterfaceName is the default name for the interface used by Divisor.
	DefaultInterfaceName = "divisor"
	// DefaultContainerPrefix is the default prefix for container names to filter.
	DefaultContainerPrefix = "quotient-runner"
	// DefaultRedisAddr is used when REDIS_ADDR is unset.
	DefaultRedisAddr = "localhost:6379"

	// EnvRedisAddr is the environment variable for Redis address.
	EnvRedisAddr = "REDIS_ADDR"
	// EnvRedisPassword is the environment variable for Redis password.
	EnvRedisPassword = "REDIS_PASSWORD"
	// EnvNumIPs is the environment variable for the number of IPs.
	EnvNumIPs = "NUM_IPS"
	// EnvTargetSubnets is the environment variable for target subnets.
	EnvTargetSubnets = "TARGET_SUBNETS"
	// EnvInterfaceName is the environment variable for the interface name.
	EnvInterfaceName = "INTERFACE_NAME"
	// EnvDesiredSubnet is the environment variable for the desired subnet(s).
	EnvDesiredSubnet = "DESIRED_SUBNET"
	// EnvDesiredSubnets is the plural alias for EnvDesiredSubnet.
	EnvDesiredSubnets = "DESIRED_SUBNETS"
	// EnvContainerPrefix is the environment variable for the container prefix.
	EnvContainerPrefix = "CONTAINER_PREFIX"
	// EnvMode is the environment variable for the mode of operation.
	EnvMode = "DIVISOR_MODE"
	// EnvTargetSubnetGateway is the environment variable for the target subnet gateway.
	EnvTargetSubnetGateway = "TARGET_SUBNET_GATEWAY"

	// ModeSNAT is the default mode of operation, SNATting runner containers from the host.
	ModeSNAT = "snat"
	// ModeDirect is the mode of operation that sets the IP of a runner directly.
	ModeDirect = "direct"
)

// Config is a snapshot of the process environment.
// InterfaceName and ContainerPrefix are not defaulted here: an empty
// interface name must still reach the promote_secondaries sysctl path.
type Config struct {
	Mode                string
	RedisAddr           string
	RedisPassword       string
	NumIPs              string
	TargetSubnets       string
	InterfaceName       string
	DesiredSubnets      string
	ContainerPrefix     string
	TargetSubnetGateway string
}

// Load reads the current environment. It does not validate the mode.
func Load() Config {
	redisAddr := os.Getenv(EnvRedisAddr)
	if redisAddr == "" {
		redisAddr = DefaultRedisAddr
	}
	return Config{
		Mode:                os.Getenv(EnvMode),
		RedisAddr:           redisAddr,
		RedisPassword:       os.Getenv(EnvRedisPassword),
		NumIPs:              os.Getenv(EnvNumIPs),
		TargetSubnets:       os.Getenv(EnvTargetSubnets),
		InterfaceName:       os.Getenv(EnvInterfaceName),
		DesiredSubnets:      desiredSubnets(),
		ContainerPrefix:     os.Getenv(EnvContainerPrefix),
		TargetSubnetGateway: os.Getenv(EnvTargetSubnetGateway),
	}
}

// ValidateMode reports an invalid DIVISOR_MODE. Unset, snat, and direct are accepted.
func (c Config) ValidateMode() error {
	m := c.Mode
	if m != "" && m != ModeSNAT && m != ModeDirect {
		return fmt.Errorf("Invalid %s %q: must be unset, %s or %s", EnvMode, m, ModeSNAT, ModeDirect)
	}
	return nil
}

// InterfaceNameOrDefault applies the default interface name at the use site.
func (c Config) InterfaceNameOrDefault() string {
	if c.InterfaceName == "" {
		return DefaultInterfaceName
	}
	return c.InterfaceName
}

// ContainerPrefixOrDefault applies the default container prefix at the use site.
func (c Config) ContainerPrefixOrDefault() string {
	if c.ContainerPrefix == "" {
		return DefaultContainerPrefix
	}
	return c.ContainerPrefix
}

func desiredSubnets() string {
	if v := strings.TrimSpace(os.Getenv(EnvDesiredSubnets)); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(EnvDesiredSubnet))
}
