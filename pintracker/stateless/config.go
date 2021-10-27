package stateless

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/kelseyhightower/envconfig"

	"github.com/ipfs/ipfs-cluster/config"
)

const configKey = "stateless"
const envConfigKey = "cluster_stateless"

// Default values for this Config.
const (
	DefaultMaxPinQueueSize       = 1000000
	DefaultConcurrentPins        = 10
	DefaultPriorityPinMaxAge     = 24 * time.Hour
	DefaultPriorityPinMaxRetries = 5
)

// Config allows to initialize a Monitor and customize some parameters.
type Config struct {
	config.Saver

	// If higher, they will automatically marked with an error.
	MaxPinQueueSize int
	// ConcurrentPins specifies how many pin requests can be sent to the ipfs
	// daemon in parallel. If the pinning method is "refs", it might increase
	// speed. Unpin requests are always processed one by one.
	ConcurrentPins int

	// PriorityPinMaxAge specifies the maximum age that a pin needs to
	// can have since it was submitted to the cluster to be pinned
	// preferentially (before pins that are older or have too many retries).
	PriorityPinMaxAge time.Duration

	// PriorityPinMaxRetries specifies the maximum amount of retries that
	// a pin can have before it is moved to a non-prioritary queue.
	PriorityPinMaxRetries int
}

type jsonConfig struct {
	MaxPinQueueSize       int    `json:"max_pin_queue_size,omitempty"`
	ConcurrentPins        int    `json:"concurrent_pins"`
	PriorityPinMaxAge     string `json:"priority_pin_max_age"`
	PriorityPinMaxRetries int    `json:"priority_pin_max_retries"`
}

// ConfigKey provides a human-friendly identifier for this type of Config.
func (cfg *Config) ConfigKey() string {
	return configKey
}

// Default sets the fields of this Config to sensible values.
func (cfg *Config) Default() error {
	cfg.MaxPinQueueSize = DefaultMaxPinQueueSize
	cfg.ConcurrentPins = DefaultConcurrentPins
	cfg.PriorityPinMaxAge = DefaultPriorityPinMaxAge
	cfg.PriorityPinMaxRetries = DefaultPriorityPinMaxRetries
	return nil
}

// ApplyEnvVars fills in any Config fields found
// as environment variables.
func (cfg *Config) ApplyEnvVars() error {
	jcfg := cfg.toJSONConfig()

	err := envconfig.Process(envConfigKey, jcfg)
	if err != nil {
		return err
	}

	return cfg.applyJSONConfig(jcfg)
}

// Validate checks that the fields of this Config have working values,
// at least in appearance.
func (cfg *Config) Validate() error {
	if cfg.MaxPinQueueSize <= 0 {
		return errors.New("statelesstracker.max_pin_queue_size too low")
	}

	if cfg.ConcurrentPins <= 0 {
		return errors.New("statelesstracker.concurrent_pins is too low")
	}

	if cfg.PriorityPinMaxAge <= 0 {
		return errors.New("statelesstracker.priority_pin_max_age is too low")
	}

	if cfg.PriorityPinMaxRetries <= 0 {
		return errors.New("statelesstracker.priority_pin_max_retries is too low")
	}

	return nil
}

// LoadJSON sets the fields of this Config to the values defined by the JSON
// representation of it, as generated by ToJSON.
func (cfg *Config) LoadJSON(raw []byte) error {
	jcfg := &jsonConfig{}
	err := json.Unmarshal(raw, jcfg)
	if err != nil {
		logger.Error("Error unmarshaling statelesstracker config")
		return err
	}

	cfg.Default()

	return cfg.applyJSONConfig(jcfg)
}

func (cfg *Config) applyJSONConfig(jcfg *jsonConfig) error {
	config.SetIfNotDefault(jcfg.MaxPinQueueSize, &cfg.MaxPinQueueSize)
	config.SetIfNotDefault(jcfg.ConcurrentPins, &cfg.ConcurrentPins)
	err := config.ParseDurations(cfg.ConfigKey(),
		&config.DurationOpt{
			Duration: jcfg.PriorityPinMaxAge,
			Dst:      &cfg.PriorityPinMaxAge,
			Name:     "priority_pin_max_age",
		},
	)
	if err != nil {
		return err
	}

	config.SetIfNotDefault(jcfg.PriorityPinMaxRetries, &cfg.PriorityPinMaxRetries)

	return cfg.Validate()
}

// ToJSON generates a human-friendly JSON representation of this Config.
func (cfg *Config) ToJSON() ([]byte, error) {
	jcfg := cfg.toJSONConfig()

	return config.DefaultJSONMarshal(jcfg)
}

func (cfg *Config) toJSONConfig() *jsonConfig {
	jCfg := &jsonConfig{
		ConcurrentPins:        cfg.ConcurrentPins,
		PriorityPinMaxAge:     cfg.PriorityPinMaxAge.String(),
		PriorityPinMaxRetries: cfg.PriorityPinMaxRetries,
	}
	if cfg.MaxPinQueueSize != DefaultMaxPinQueueSize {
		jCfg.MaxPinQueueSize = cfg.MaxPinQueueSize
	}

	return jCfg
}

// ToDisplayJSON returns JSON config as a string.
func (cfg *Config) ToDisplayJSON() ([]byte, error) {
	return config.DisplayJSON(cfg.toJSONConfig())
}
