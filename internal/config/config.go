package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/vectorcore/gmlc/internal/domain"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    Server    `yaml:"server"`
	Database  Database  `yaml:"database"`
	Retention Retention `yaml:"retention"`
	Clients   []Client  `yaml:"clients"`
	Diameter  Diameter  `yaml:"diameter"`
	Logging   Logging   `yaml:"logging"`
}
type Logging struct {
	File  string `yaml:"file"`
	Level string `yaml:"level"`
}
type Diameter struct {
	OriginHost        string         `yaml:"origin_host"`
	OriginRealm       string         `yaml:"origin_realm"`
	HostIPAddress     string         `yaml:"host_ip_address"`
	HSSRealm          string         `yaml:"hss_realm"`
	HSSHost           string         `yaml:"hss_host"`
	ConnectionTimeout time.Duration  `yaml:"connection_timeout"`
	RequestTimeout    time.Duration  `yaml:"request_timeout"`
	ReconnectMin      time.Duration  `yaml:"reconnect_min"`
	ReconnectMax      time.Duration  `yaml:"reconnect_max"`
	WatchdogInterval  time.Duration  `yaml:"watchdog_interval"`
	WatchdogTimeout   time.Duration  `yaml:"watchdog_timeout"`
	ShutdownTimeout   time.Duration  `yaml:"shutdown_timeout"`
	Peers             []DiameterPeer `yaml:"peers"`
}
type DiameterPeer struct {
	Name                string `yaml:"name"`
	Address             string `yaml:"address"`
	Transport           string `yaml:"transport"`
	ExpectedOriginHost  string `yaml:"expected_origin_host"`
	ExpectedOriginRealm string `yaml:"expected_origin_realm"`
}
type Server struct {
	ListenAddress   string        `yaml:"listen_address"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}
type Database struct {
	Path            string        `yaml:"path"`
	BusyTimeout     time.Duration `yaml:"busy_timeout"`
	Synchronous     string        `yaml:"synchronous"`
	CheckpointPages int           `yaml:"checkpoint_pages"`
}
type Retention struct {
	Request       time.Duration `yaml:"request"`
	Result        time.Duration `yaml:"result"`
	PurgeInterval time.Duration `yaml:"purge_interval"`
}
type Client struct {
	ID             string               `yaml:"id"`
	BearerToken    string               `yaml:"bearer_token"`
	Services       []domain.ServiceType `yaml:"services"`
	TargetPrefixes []string             `yaml:"target_prefixes"`
	// LCSClientType selects the TS 29.172 LCS-Client-Type this client's
	// requests are tagged with. Operator-controlled only — never settable via
	// the REST API — since EMERGENCY_SERVICES/LAWFUL_INTERCEPT_SERVICES carry
	// regulatory weight downstream. One of: emergency_services,
	// value_added_services (default), plmn_operator_services,
	// lawful_intercept_services.
	LCSClientType string `yaml:"lcs_client_type"`
}

var clientTypeValues = map[string]uint32{
	"emergency_services":        domain.ClientTypeEmergencyServices,
	"value_added_services":      domain.ClientTypeValueAddedServices,
	"plmn_operator_services":    domain.ClientTypePLMNOperatorServices,
	"lawful_intercept_services": domain.ClientTypeLawfulIntercept,
}

// ClientTypeValue resolves the configured LCSClientType name to its TS
// 29.172 numeric value. LCSClientType is validated by Config.Validate, so
// this is only called on an already-validated config.
func (c Client) ClientTypeValue() uint32 { return clientTypeValues[c.LCSClientType] }

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err = rejectObsoleteDiameterKeys(b); err != nil {
		return c, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err = dec.Decode(&c); err != nil {
		return c, err
	}
	c.applyDefaults()
	return c, c.Validate()
}
func (c *Config) applyDefaults() {
	if c.Server.ShutdownTimeout <= 0 {
		c.Server.ShutdownTimeout = 10 * time.Second
	}
	if c.Database.BusyTimeout <= 0 {
		c.Database.BusyTimeout = 5 * time.Second
	}
	if c.Database.Synchronous == "" {
		c.Database.Synchronous = "FULL"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Retention.PurgeInterval <= 0 {
		c.Retention.PurgeInterval = time.Hour
	}
	// HSS realm defaults to the GMLC's own realm, matching the common
	// same-operator deployment where GMLC and HSS share a Diameter realm.
	// diameter.hss_realm lets cross-realm/interconnect deployments override it.
	if c.Diameter.HSSRealm == "" {
		c.Diameter.HSSRealm = c.Diameter.OriginRealm
	}
	for i := range c.Clients {
		if c.Clients[i].LCSClientType == "" {
			c.Clients[i].LCSClientType = "value_added_services"
		}
	}
}
func (c Config) Validate() error {
	if c.Logging.File == "" {
		return fmt.Errorf("logging.file is required")
	}
	switch strings.ToLower(c.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level must be debug, info, warn, or error")
	}
	if c.Server.ListenAddress == "" {
		return fmt.Errorf("server.listen_address is required")
	}
	if c.Database.Path == "" {
		return fmt.Errorf("database.path is required")
	}
	if c.Database.CheckpointPages <= 0 {
		return fmt.Errorf("database.checkpoint_pages must be positive")
	}
	if c.Retention.Request <= 0 || c.Retention.Result <= 0 {
		return fmt.Errorf("retention values must be positive")
	}
	if len(c.Clients) == 0 {
		return fmt.Errorf("at least one explicitly configured client is required")
	}
	d := c.Diameter
	if d.OriginHost == "" || d.OriginRealm == "" || d.HostIPAddress == "" || len(d.Peers) == 0 {
		return fmt.Errorf("diameter origin identity and at least one peer are required")
	}
	if d.ConnectionTimeout <= 0 || d.RequestTimeout <= 0 || d.ReconnectMin <= 0 || d.ReconnectMax < d.ReconnectMin || d.WatchdogInterval <= 0 || d.WatchdogTimeout <= 0 || d.ShutdownTimeout <= 0 {
		return fmt.Errorf("invalid diameter timeouts/backoff")
	}
	peers := map[string]bool{}
	names := map[string]bool{}
	for i, p := range d.Peers {
		if p.Name == "" || p.Address == "" || (p.Transport != "tcp" && p.Transport != "sctp") {
			return fmt.Errorf("diameter.peers[%d]: name, address and transport (tcp or sctp) are required", i)
		}
		k := strings.ToLower(p.Transport) + "\x00" + strings.ToLower(p.Address)
		if peers[k] {
			return fmt.Errorf("duplicate diameter peer %s/%s", p.Transport, p.Address)
		}
		if names[strings.ToLower(p.Name)] {
			return fmt.Errorf("duplicate diameter peer name %q", p.Name)
		}
		peers[k] = true
		names[strings.ToLower(p.Name)] = true
	}
	seen := map[string]bool{}
	for _, v := range c.Clients {
		if v.ID == "" || v.BearerToken == "" || seen[v.ID] {
			return fmt.Errorf("each client needs unique id and bearer_token")
		}
		seen[v.ID] = true
		if len(v.Services) == 0 || len(v.TargetPrefixes) == 0 {
			return fmt.Errorf("client %q needs services and target_prefixes", v.ID)
		}
		for _, p := range v.TargetPrefixes {
			if strings.Trim(p, "0123456789") != "" {
				return fmt.Errorf("client %q prefix is not numeric", v.ID)
			}
		}
		if _, ok := clientTypeValues[v.LCSClientType]; !ok {
			return fmt.Errorf("client %q has invalid lcs_client_type %q", v.ID, v.LCSClientType)
		}
	}
	return nil
}

func rejectObsoleteDiameterKeys(b []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(root.Content[0].Content); i += 2 {
		if root.Content[0].Content[i].Value != "diameter" {
			continue
		}
		d := root.Content[0].Content[i+1]
		if d.Kind != yaml.MappingNode {
			return nil
		}
		forbidden := map[string]bool{"enabled": true, "mode": true, "dra": true, "direct": true, "product_name": true, "vendor_id": true, "peer_mode": true, "peer_address": true, "peer_port": true, "transport": true, "destination_host": true, "destination_realm": true, "hss": true, "mme": true, "mmes": true, "slh": true, "slg": true}
		for j := 0; j < len(d.Content); j += 2 {
			if forbidden[d.Content[j].Value] {
				return fmt.Errorf("diameter.%s is obsolete and rejected; configure diameter.peers", d.Content[j].Value)
			}
		}
	}
	return nil
}
