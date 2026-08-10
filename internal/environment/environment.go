package environment

import (
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/sshcheck"
	"gopkg.in/yaml.v3"
)

var (
	idPattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
	serverPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$`)
	hostPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)
	imagePattern  = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
	envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type Config struct {
	Version      int                 `yaml:"version"`
	Cloudflare   *CloudflareConfig   `yaml:"cloudflare"`
	ReverseProxy ReverseProxyConfig  `yaml:"reverseProxy"`
	Servers      map[string]Server   `yaml:"servers"`
	Postgres     map[string]Postgres `yaml:"postgres"`
	Redis        map[string]Redis    `yaml:"redis"`
	Apps         map[string]App      `yaml:"apps"`
}

type CloudflareConfig struct {
	ZoneID string `yaml:"zoneId"`
}
type ReverseProxyConfig struct {
	Image     string `yaml:"image"`
	AcmeEmail string `yaml:"acmeEmail"`
}
type Server struct {
	Addresses   *Addresses `yaml:"addresses"`
	SingBox     *SingBox   `yaml:"singBox"`
	SSHAlias    string     `yaml:"sshAlias"`
	sshAliasSet bool
}
type Addresses struct {
	Public   *AddressSet `yaml:"public"`
	Internal *AddressSet `yaml:"internal"`
}
type AddressSet struct {
	IPv4 string `yaml:"ipv4"`
	IPv6 string `yaml:"ipv6"`
}
type SingBox struct {
	ServerName string `yaml:"serverName"`
	Target     string `yaml:"target"`
}
type Postgres struct {
	Type                                                       string     `yaml:"type"`
	Server                                                     string     `yaml:"server"`
	Host                                                       string     `yaml:"host"`
	Port                                                       *int       `yaml:"port"`
	TLS                                                        *TLSConfig `yaml:"tls"`
	Region                                                     string     `yaml:"region"`
	Compute                                                    *Compute   `yaml:"compute"`
	serverSet, hostSet, portSet, tlsSet, regionSet, computeSet bool
}
type TLSConfig struct {
	Mode string `yaml:"mode"`
}
type Compute struct {
	MinCU                               *float64 `yaml:"minCU"`
	MaxCU                               *float64 `yaml:"maxCU"`
	SuspendAfter                        Duration `yaml:"suspendAfter"`
	minCUSet, maxCUSet, suspendAfterSet bool
}
type Redis struct {
	Type                                                           string `yaml:"type"`
	Server                                                         string `yaml:"server"`
	Host                                                           string `yaml:"host"`
	Port                                                           *int   `yaml:"port"`
	TLS                                                            *bool  `yaml:"tls"`
	Persistence                                                    *bool  `yaml:"persistence"`
	Region                                                         string `yaml:"region"`
	serverSet, hostSet, portSet, tlsSet, persistenceSet, regionSet bool
}
type App struct {
	Hostname          string            `yaml:"hostname"`
	Image             string            `yaml:"image"`
	InitialAdminEmail string            `yaml:"initialAdminEmail"`
	ReadinessPath     string            `yaml:"readinessPath"`
	DrainTimeout      Duration          `yaml:"drainTimeout"`
	Environment       map[string]string `yaml:"environment"`
	Servers           []string          `yaml:"servers"`
	Postgres          AppPostgres       `yaml:"postgres"`
	Redis             AppRedis          `yaml:"redis"`
	PublicAccess      PublicAccess      `yaml:"publicAccess"`
	OutboundProxy     *OutboundProxy    `yaml:"outboundProxy"`
	serversSet        bool
}
type AppPostgres struct {
	Name     string `yaml:"name"`
	Database string `yaml:"database"`
}
type AppRedis struct {
	Name     string `yaml:"name"`
	Database int    `yaml:"database"`
}
type PublicAccess struct {
	Type                      string            `yaml:"type"`
	Servers                   []string          `yaml:"servers"`
	Cloudflare                *CloudflareAccess `yaml:"cloudflare"`
	serversSet, cloudflareSet bool
}
type CloudflareAccess struct {
	Mode        string       `yaml:"mode"`
	ConnectBy   string       `yaml:"connectBy"`
	HealthCheck *HealthCheck `yaml:"healthCheck"`
}
type HealthCheck struct {
	Path string `yaml:"path"`
}
type OutboundProxy struct {
	Enabled                                      bool     `yaml:"enabled"`
	Type                                         string   `yaml:"type"`
	Required                                     bool     `yaml:"required"`
	Servers                                      []string `yaml:"servers"`
	enabledSet, typeSet, requiredSet, serversSet bool
}

type Secrets struct {
	PulumiPassphrase string                     `yaml:"pulumiPassphrase"`
	Cloudflare       *CloudflareSecrets         `yaml:"cloudflare"`
	ReverseProxy     *ReverseProxySecrets       `yaml:"reverseProxy"`
	Apps             map[string]AppSecrets      `yaml:"apps"`
	Postgres         map[string]PostgresSecrets `yaml:"postgres"`
	Redis            map[string]RedisSecrets    `yaml:"redis"`
	OutboundProxy    map[string]ProxySecrets    `yaml:"outboundProxy"`
}
type CloudflareSecrets struct {
	APIToken string `yaml:"apiToken"`
}
type ReverseProxySecrets struct {
	DNSChallengeToken string `yaml:"dnsChallengeToken"`
}
type AppSecrets struct {
	InitialAdminPassword  string              `yaml:"initialAdminPassword"`
	JWTSecret             string              `yaml:"jwtSecret"`
	TOTPEncryptionKey     string              `yaml:"totpEncryptionKey"`
	AdminAPIKey           string              `yaml:"adminApiKey"`
	Environment           map[string]string   `yaml:"environment"`
	Postgres              *AppPostgresSecrets `yaml:"postgres"`
	Redis                 *AppRedisSecrets    `yaml:"redis"`
	postgresSet, redisSet bool
}
type AppPostgresSecrets struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}
type AppRedisSecrets struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}
type PostgresSecrets struct {
	AdminPassword                 string `yaml:"adminPassword"`
	APIToken                      string `yaml:"apiToken"`
	adminPasswordSet, apiTokenSet bool
}
type RedisSecrets struct {
	AdminPassword               string `yaml:"adminPassword"`
	APIKey                      string `yaml:"apiKey"`
	adminPasswordSet, apiKeySet bool
}
type ProxySecrets struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Duration struct {
	time.Duration
	Set bool
}

func (d Duration) String() string { return d.Duration.String() }
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("duration must be a YAML string")
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration")
	}
	d.Duration = parsed
	d.Set = true
	return nil
}

func knownMappingFields(node *yaml.Node, allowed map[string]bool) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("must be a YAML object")
	}
	for index := 0; index < len(node.Content); index += 2 {
		if !allowed[node.Content[index].Value] {
			return fmt.Errorf("field %q is unsupported", node.Content[index].Value)
		}
	}
	return nil
}

func hasMappingField(node *yaml.Node, name string) bool {
	if node.Kind != yaml.MappingNode {
		return false
	}
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == name {
			return true
		}
	}
	return false
}

func (p *Postgres) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"type": true, "server": true, "host": true, "port": true, "tls": true, "region": true, "compute": true}); err != nil {
		return err
	}
	type plain Postgres
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*p = Postgres(value)
	p.serverSet, p.hostSet, p.portSet, p.tlsSet, p.regionSet, p.computeSet = hasMappingField(node, "server"), hasMappingField(node, "host"), hasMappingField(node, "port"), hasMappingField(node, "tls"), hasMappingField(node, "region"), hasMappingField(node, "compute")
	if p.computeSet && p.Compute == nil {
		return fmt.Errorf("compute must not be null")
	}
	return nil
}

func (c *Compute) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"minCU": true, "maxCU": true, "suspendAfter": true}); err != nil {
		return err
	}
	for index := 0; index < len(node.Content); index += 2 {
		field, value := node.Content[index], node.Content[index+1]
		if (field.Value == "minCU" || field.Value == "maxCU" || field.Value == "suspendAfter") && value.Tag == "!!null" {
			return fmt.Errorf("compute fields must not be null")
		}
	}
	type plain Compute
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*c = Compute(value)
	c.minCUSet, c.maxCUSet, c.suspendAfterSet = hasMappingField(node, "minCU"), hasMappingField(node, "maxCU"), hasMappingField(node, "suspendAfter")
	return nil
}

func (r *Redis) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"type": true, "server": true, "host": true, "port": true, "tls": true, "persistence": true, "region": true}); err != nil {
		return err
	}
	type plain Redis
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*r = Redis(value)
	r.serverSet, r.hostSet, r.portSet, r.tlsSet, r.persistenceSet, r.regionSet = hasMappingField(node, "server"), hasMappingField(node, "host"), hasMappingField(node, "port"), hasMappingField(node, "tls"), hasMappingField(node, "persistence"), hasMappingField(node, "region")
	return nil
}

func (a *App) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"hostname": true, "image": true, "initialAdminEmail": true, "readinessPath": true, "drainTimeout": true, "environment": true, "servers": true, "postgres": true, "redis": true, "publicAccess": true, "outboundProxy": true}); err != nil {
		return err
	}
	for index := 0; index < len(node.Content); index += 2 {
		if node.Content[index].Value == "servers" && node.Content[index+1].Kind != yaml.SequenceNode {
			return fmt.Errorf("servers must be a YAML sequence")
		}
	}
	type plain App
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*a = App(value)
	a.serversSet = hasMappingField(node, "servers")
	if hasMappingField(node, "outboundProxy") && a.OutboundProxy == nil {
		return fmt.Errorf("outboundProxy must not be null")
	}
	return nil
}

func (s *Server) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"addresses": true, "singBox": true, "sshAlias": true}); err != nil {
		return err
	}
	type plain Server
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*s = Server(value)
	s.sshAliasSet = hasMappingField(node, "sshAlias")
	return nil
}

func (p *PublicAccess) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"type": true, "servers": true, "cloudflare": true}); err != nil {
		return err
	}
	type plain PublicAccess
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*p = PublicAccess(value)
	p.serversSet, p.cloudflareSet = hasMappingField(node, "servers"), hasMappingField(node, "cloudflare")
	return nil
}

func (p *OutboundProxy) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"enabled": true, "type": true, "required": true, "servers": true}); err != nil {
		return err
	}
	for index := 0; index < len(node.Content); index += 2 {
		field, value := node.Content[index], node.Content[index+1]
		if (field.Value == "enabled" || field.Value == "required") && value.Tag == "!!null" {
			return fmt.Errorf("outboundProxy.%s must not be null", field.Value)
		}
	}
	type plain OutboundProxy
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*p = OutboundProxy(value)
	p.enabledSet, p.typeSet, p.requiredSet, p.serversSet = hasMappingField(node, "enabled"), hasMappingField(node, "type"), hasMappingField(node, "required"), hasMappingField(node, "servers")
	return nil
}

func (t *TLSConfig) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"mode": true}); err != nil {
		return err
	}
	type plain TLSConfig
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*t = TLSConfig(value)
	return nil
}

func (p *AppPostgres) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"name": true, "database": true}); err != nil {
		return err
	}
	type plain AppPostgres
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*p = AppPostgres(value)
	return nil
}

func (r *AppRedis) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"name": true, "database": true}); err != nil {
		return err
	}
	type plain AppRedis
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*r = AppRedis(value)
	return nil
}

func (c *CloudflareAccess) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"mode": true, "connectBy": true, "healthCheck": true}); err != nil {
		return err
	}
	type plain CloudflareAccess
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*c = CloudflareAccess(value)
	return nil
}

func (h *HealthCheck) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"path": true}); err != nil {
		return err
	}
	type plain HealthCheck
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*h = HealthCheck(value)
	return nil
}

func (a *AppSecrets) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"initialAdminPassword": true, "jwtSecret": true, "totpEncryptionKey": true, "adminApiKey": true, "environment": true, "postgres": true, "redis": true}); err != nil {
		return err
	}
	type plain AppSecrets
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*a = AppSecrets(value)
	a.postgresSet, a.redisSet = hasMappingField(node, "postgres"), hasMappingField(node, "redis")
	return nil
}

func (p *AppPostgresSecrets) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"username": true, "password": true}); err != nil {
		return err
	}
	type plain AppPostgresSecrets
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*p = AppPostgresSecrets(value)
	return nil
}

func (r *AppRedisSecrets) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"username": true, "password": true}); err != nil {
		return err
	}
	type plain AppRedisSecrets
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*r = AppRedisSecrets(value)
	return nil
}

func (p *PostgresSecrets) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"adminPassword": true, "apiToken": true}); err != nil {
		return err
	}
	type plain PostgresSecrets
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*p = PostgresSecrets(value)
	p.adminPasswordSet, p.apiTokenSet = hasMappingField(node, "adminPassword"), hasMappingField(node, "apiToken")
	return nil
}

func (r *RedisSecrets) UnmarshalYAML(node *yaml.Node) error {
	if err := knownMappingFields(node, map[string]bool{"adminPassword": true, "apiKey": true}); err != nil {
		return err
	}
	type plain RedisSecrets
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*r = RedisSecrets(value)
	r.adminPasswordSet, r.apiKeySet = hasMappingField(node, "adminPassword"), hasMappingField(node, "apiKey")
	return nil
}

type ValidatedConfig struct {
	Config
	ServerIDs  []string
	SSHAliases []string
}
type EnvironmentPaths struct{ Root, Directory, Config, Secrets string }

func ParseConfig(data []byte) (Config, error) {
	var value Config
	if err := decodeStrict(data, &value); err != nil {
		return Config{}, err
	}
	return value, nil
}
func ParseSecrets(data []byte) (Secrets, error) {
	var value Secrets
	if err := decodeStrict(data, &value); err != nil {
		return Secrets{}, err
	}
	return value, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	if err := rejectDuplicateKeys(&document); err != nil {
		return err
	}
	strictDecoder := yaml.NewDecoder(strings.NewReader(string(data)))
	strictDecoder.KnownFields(true)
	if err := strictDecoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := strictDecoder.Decode(&extra); err == nil {
		return fmt.Errorf("YAML must contain exactly one document")
	} else if err != io.EOF {
		return err
	}
	return nil
}

func rejectDuplicateKeys(node *yaml.Node) error {
	if node.Kind == yaml.DocumentNode {
		for _, child := range node.Content {
			if err := rejectDuplicateKeys(child); err != nil {
				return err
			}
		}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		for _, child := range node.Content {
			if err := rejectDuplicateKeys(child); err != nil {
				return err
			}
		}
		return nil
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		if _, exists := seen[key.Value]; exists {
			return fmt.Errorf("duplicate YAML field %q", key.Value)
		}
		seen[key.Value] = struct{}{}
		if err := rejectDuplicateKeys(value); err != nil {
			return err
		}
	}
	return nil
}

func Validate(config Config, secrets Secrets) (ValidatedConfig, error) {
	if config.Version != 1 {
		return ValidatedConfig{}, fmt.Errorf("version must be 1")
	}
	if config.ReverseProxy.Image == "" || config.ReverseProxy.AcmeEmail == "" {
		return ValidatedConfig{}, fmt.Errorf("reverseProxy.image and reverseProxy.acmeEmail are required")
	}
	if len(config.Servers) == 0 || len(config.Apps) == 0 || len(config.Postgres) == 0 || len(config.Redis) == 0 {
		return ValidatedConfig{}, fmt.Errorf("servers, postgres, redis, and apps are required")
	}
	for id := range config.Servers {
		if !serverPattern.MatchString(id) {
			return ValidatedConfig{}, fmt.Errorf("server ID %q is invalid", id)
		}
		server := config.Servers[id]
		if !server.sshAliasSet {
			server.SSHAlias = id
		}
		if err := sshcheck.ValidateAlias(server.SSHAlias); err != nil {
			return ValidatedConfig{}, fmt.Errorf("servers.%s.sshAlias: %w", id, err)
		}
		if err := validateServer(server); err != nil {
			return ValidatedConfig{}, fmt.Errorf("servers.%s: %w", id, err)
		}
		config.Servers[id] = server
	}
	for id := range config.Postgres {
		if !idPattern.MatchString(id) {
			return ValidatedConfig{}, fmt.Errorf("postgres ID %q is invalid", id)
		}
	}
	for id := range config.Redis {
		if !idPattern.MatchString(id) {
			return ValidatedConfig{}, fmt.Errorf("redis ID %q is invalid", id)
		}
	}
	for id := range config.Apps {
		if !idPattern.MatchString(id) {
			return ValidatedConfig{}, fmt.Errorf("app ID %q is invalid", id)
		}
	}
	if err := validateServices(config); err != nil {
		return ValidatedConfig{}, err
	}
	cloudflareUsed := false
	for appID, app := range config.Apps {
		if err := validateApp(appID, app, config); err != nil {
			return ValidatedConfig{}, err
		}
		if app.PublicAccess.Type == "cloudflare" {
			cloudflareUsed = true
		}
	}
	if cloudflareUsed {
		if config.Cloudflare == nil || config.Cloudflare.ZoneID == "" {
			return ValidatedConfig{}, fmt.Errorf("cloudflare.zoneId is required when an App uses Cloudflare")
		}
		if secrets.Cloudflare == nil || secrets.Cloudflare.APIToken == "" {
			return ValidatedConfig{}, fmt.Errorf("cloudflare.apiToken is required when an App uses Cloudflare")
		}
	}
	if !cloudflareUsed && (config.Cloudflare != nil || secrets.Cloudflare != nil) {
		return ValidatedConfig{}, fmt.Errorf("cloudflare settings are only valid when an App uses Cloudflare")
	}
	if secrets.ReverseProxy == nil || secrets.ReverseProxy.DNSChallengeToken == "" {
		return ValidatedConfig{}, fmt.Errorf("reverseProxy.dnsChallengeToken is required")
	}
	if err := validateSecrets(config, secrets); err != nil {
		return ValidatedConfig{}, err
	}
	ids := make([]string, 0, len(config.Servers))
	aliases := make([]string, 0, len(config.Servers))
	seenAliases := make(map[string]string, len(config.Servers))
	for id := range config.Servers {
		ids = append(ids, id)
		alias := config.Servers[id].SSHAlias
		if other, exists := seenAliases[alias]; exists {
			return ValidatedConfig{}, fmt.Errorf("servers.%s.sshAlias duplicates servers.%s.sshAlias", id, other)
		}
		seenAliases[alias] = id
		aliases = append(aliases, alias)
	}
	sort.Strings(ids)
	sort.Strings(aliases)
	return ValidatedConfig{Config: config, ServerIDs: ids, SSHAliases: aliases}, nil
}

func validateServer(server Server) error {
	if server.Addresses != nil {
		for name, set := range map[string]*AddressSet{"public": server.Addresses.Public, "internal": server.Addresses.Internal} {
			if set == nil {
				continue
			}
			if set.IPv4 == "" && set.IPv6 == "" {
				return fmt.Errorf("addresses.%s must contain an address", name)
			}
			if set.IPv4 != "" && (net.ParseIP(set.IPv4) == nil || strings.Contains(set.IPv4, ":")) {
				return fmt.Errorf("addresses.%s.ipv4 is invalid", name)
			}
			if set.IPv6 != "" && (net.ParseIP(set.IPv6) == nil || !strings.Contains(set.IPv6, ":")) {
				return fmt.Errorf("addresses.%s.ipv6 is invalid", name)
			}
		}
	}
	if server.SingBox != nil {
		if !validHostname(server.SingBox.ServerName) {
			return fmt.Errorf("singBox.serverName must be a lowercase DNS hostname")
		}
		if err := validateHostPort(server.SingBox.Target); err != nil {
			return fmt.Errorf("singBox.target: %w", err)
		}
	}
	return nil
}

func validateServices(config Config) error {
	for id, service := range config.Postgres {
		name := "postgres." + id
		switch service.Type {
		case "docker":
			if service.Server == "" {
				return fmt.Errorf("%s.server is required", name)
			}
			if service.hostSet || service.tlsSet || service.regionSet || service.computeSet {
				return fmt.Errorf("%s contains fields not valid for docker", name)
			}
			if _, ok := config.Servers[service.Server]; !ok {
				return fmt.Errorf("%s.server references missing server %q", name, service.Server)
			}
			if service.Port == nil {
				port := 5432
				service.Port = &port
				config.Postgres[id] = service
			}
		case "external":
			if service.serverSet || service.regionSet || service.computeSet {
				return fmt.Errorf("%s contains fields not valid for external", name)
			}
			if service.Host == "" || !service.hostSet {
				return fmt.Errorf("%s.host is required", name)
			}
			if service.Host != strings.ToLower(service.Host) || !validHostname(service.Host) {
				return fmt.Errorf("%s.host must be a lowercase DNS hostname", name)
			}
			if service.TLS != nil && service.TLS.Mode != "disable" && service.TLS.Mode != "require" && service.TLS.Mode != "verify-ca" && service.TLS.Mode != "verify-full" {
				return fmt.Errorf("%s.tls.mode is invalid", name)
			}
			if service.Port == nil {
				port := 5432
				service.Port = &port
				config.Postgres[id] = service
			}
		case "neon":
			if service.serverSet || service.hostSet || service.portSet || service.tlsSet {
				return fmt.Errorf("%s contains fields not valid for neon", name)
			}
			if service.regionSet && service.Region == "" {
				return fmt.Errorf("%s.region must not be empty", name)
			}
			if !service.regionSet {
				service.Region = "aws-us-east-1"
			}
			if service.Compute != nil {
				if !service.Compute.minCUSet || !service.Compute.maxCUSet || !service.Compute.suspendAfterSet {
					return fmt.Errorf("%s.compute requires minCU, maxCU, and suspendAfter", name)
				}
				minCU, maxCU := .25, .25
				if service.Compute.MinCU != nil {
					minCU = *service.Compute.MinCU
				}
				if service.Compute.MaxCU != nil {
					maxCU = *service.Compute.MaxCU
				}
				if math.IsNaN(minCU) || math.IsInf(minCU, 0) || math.IsNaN(maxCU) || math.IsInf(maxCU, 0) || math.IsNaN(service.Compute.SuspendAfter.Duration.Seconds()) || math.IsInf(service.Compute.SuspendAfter.Duration.Seconds(), 0) || minCU < 0.25 || minCU > 16 || maxCU < minCU || maxCU > 16 || service.Compute.SuspendAfter.Duration < time.Minute || service.Compute.SuspendAfter.Duration > 168*time.Hour {
					return fmt.Errorf("%s.compute is outside allowed ranges", name)
				}
				service.Compute.MinCU = &minCU
				service.Compute.MaxCU = &maxCU
			}
			config.Postgres[id] = service
		default:
			return fmt.Errorf("%s.type must be docker, external, or neon", name)
		}
		if service.Port != nil && (*service.Port < 1 || *service.Port > 65535) {
			return fmt.Errorf("%s.port must be between 1 and 65535", name)
		}
	}
	for id, service := range config.Redis {
		name := "redis." + id
		switch service.Type {
		case "docker":
			if service.Server == "" {
				return fmt.Errorf("%s.server is required", name)
			}
			if service.hostSet || service.regionSet || service.tlsSet {
				return fmt.Errorf("%s contains fields not valid for docker", name)
			}
			if _, ok := config.Servers[service.Server]; !ok {
				return fmt.Errorf("%s.server references missing server %q", name, service.Server)
			}
			if service.Port == nil {
				port := 6379
				service.Port = &port
				config.Redis[id] = service
			}
			if service.Persistence == nil {
				value := true
				service.Persistence = &value
				config.Redis[id] = service
			}
		case "external":
			if service.serverSet || service.regionSet || service.persistenceSet {
				return fmt.Errorf("%s contains fields not valid for external", name)
			}
			if service.Host == "" || !service.hostSet {
				return fmt.Errorf("%s.host is required", name)
			}
			if service.Host != strings.ToLower(service.Host) || !validHostname(service.Host) {
				return fmt.Errorf("%s.host must be a lowercase DNS hostname", name)
			}
			if service.Port == nil {
				port := 6379
				service.Port = &port
				config.Redis[id] = service
			}
		case "upstash":
			if service.serverSet || service.hostSet || service.portSet || service.tlsSet || service.persistenceSet {
				return fmt.Errorf("%s contains fields not valid for upstash", name)
			}
			if service.regionSet && service.Region == "" {
				return fmt.Errorf("%s.region must not be empty", name)
			}
			if !service.regionSet {
				service.Region = "us-east-1"
			}
			config.Redis[id] = service
		default:
			return fmt.Errorf("%s.type must be docker, external, or upstash", name)
		}
		if service.Port != nil && (*service.Port < 1 || *service.Port > 65535) {
			return fmt.Errorf("%s.port must be between 1 and 65535", name)
		}
	}
	return nil
}

func validateApp(id string, app App, config Config) error {
	name := "apps." + id
	if !validHostname(app.Hostname) {
		return fmt.Errorf("%s.hostname must be a lowercase DNS hostname", name)
	}
	if !imagePattern.MatchString(app.Image) {
		return fmt.Errorf("%s.image must contain an immutable sha256 digest", name)
	}
	if app.InitialAdminEmail == "" || !strings.HasPrefix(app.ReadinessPath, "/") || app.ReadinessPath == "/health" {
		return fmt.Errorf("%s has invalid required email or readinessPath", name)
	}
	if !app.DrainTimeout.Set {
		app.DrainTimeout = Duration{Duration: 10 * time.Second, Set: true}
		config.Apps[id] = app
	} else if app.DrainTimeout.Duration < 0 {
		return fmt.Errorf("%s.drainTimeout must be non-negative", name)
	}
	if err := validateEnv(name+".environment", app.Environment, nil); err != nil {
		return err
	}
	if !app.serversSet || !unique(app.Servers) {
		return fmt.Errorf("%s.servers must be specified and unique", name)
	}
	if len(app.Servers) == 0 && app.PublicAccess.Type != "none" {
		return fmt.Errorf("%s.servers may be empty only when publicAccess.type is none", name)
	}
	for _, server := range app.Servers {
		if _, ok := config.Servers[server]; !ok {
			return fmt.Errorf("%s.servers references missing server %q", name, server)
		}
	}
	if app.Postgres.Name == "" || app.Postgres.Database == "" {
		return fmt.Errorf("%s.postgres name and database are required", name)
	}
	if _, ok := config.Postgres[app.Postgres.Name]; !ok {
		return fmt.Errorf("%s.postgres.name references missing service %q", name, app.Postgres.Name)
	}
	if app.Redis.Name == "" || app.Redis.Database < 0 || app.Redis.Database > 15 {
		return fmt.Errorf("%s.redis is invalid", name)
	}
	if _, ok := config.Redis[app.Redis.Name]; !ok {
		return fmt.Errorf("%s.redis.name references missing service %q", name, app.Redis.Name)
	}
	access := app.PublicAccess
	switch access.Type {
	case "none":
		if access.serversSet || access.cloudflareSet {
			return fmt.Errorf("%s.publicAccess.none forbids servers and cloudflare", name)
		}
	case "external":
		if len(access.Servers) == 0 || !unique(access.Servers) || access.cloudflareSet {
			return fmt.Errorf("%s.publicAccess.external requires servers and forbids cloudflare", name)
		}
	case "cloudflare":
		if len(access.Servers) == 0 || !unique(access.Servers) || access.Cloudflare == nil {
			return fmt.Errorf("%s.publicAccess.cloudflare requires servers and cloudflare settings", name)
		}
	default:
		return fmt.Errorf("%s.publicAccess.type is invalid", name)
	}
	for _, server := range access.Servers {
		if !contains(app.Servers, server) {
			return fmt.Errorf("%s.publicAccess.servers must be an App server subset", name)
		}
		if access.Type == "cloudflare" && access.Cloudflare.ConnectBy == "publicAddress" && !hasPublicAddress(config.Servers[server]) {
			return fmt.Errorf("%s.publicAccess server %q lacks a public address", name, server)
		}
	}
	if access.Cloudflare != nil {
		if err := validateCloudflareAccess(name, access.Cloudflare); err != nil {
			return err
		}
	}
	if app.OutboundProxy != nil {
		p := app.OutboundProxy
		if !p.enabledSet {
			return fmt.Errorf("%s.outboundProxy.enabled is required", name)
		}
		if !p.Enabled {
			if p.typeSet || p.serversSet || (p.requiredSet && p.Required) {
				return fmt.Errorf("%s.outboundProxy disabled fields are forbidden", name)
			}
			return validateCrossServerAddresses(id, app, config)
		}
		if !p.typeSet || !p.serversSet || p.Type != "microsocks" || len(p.Servers) == 0 || !unique(p.Servers) {
			return fmt.Errorf("%s.outboundProxy is invalid", name)
		}
		for _, server := range p.Servers {
			if _, ok := config.Servers[server]; !ok {
				return fmt.Errorf("%s.outboundProxy references missing server %q", name, server)
			}
		}
	}
	return validateCrossServerAddresses(id, app, config)
}

func validateCloudflareAccess(name string, access *CloudflareAccess) error {
	switch access.Mode {
	case "dns":
		if access.ConnectBy != "publicAddress" || access.HealthCheck != nil {
			return fmt.Errorf("%s.publicAccess.cloudflare dns settings are invalid", name)
		}
	case "loadBalancer":
		if access.ConnectBy != "publicAddress" && access.ConnectBy != "tunnel" {
			return fmt.Errorf("%s.publicAccess.cloudflare loadBalancer connectBy is invalid", name)
		}
	case "tunnel":
		if access.ConnectBy != "" || access.HealthCheck != nil {
			return fmt.Errorf("%s.publicAccess.cloudflare tunnel settings are invalid", name)
		}
	default:
		return fmt.Errorf("%s.publicAccess.cloudflare.mode is invalid", name)
	}
	if access.HealthCheck != nil && !strings.HasPrefix(access.HealthCheck.Path, "/") {
		return fmt.Errorf("%s.publicAccess.cloudflare.healthCheck.path must be absolute", name)
	}
	return nil
}

func validateCrossServerAddresses(appID string, app App, config Config) error {
	if len(app.Servers) == 0 {
		return nil
	}
	servers := map[string]bool{}
	for _, server := range app.Servers {
		servers[server] = true
	}
	if service := config.Postgres[app.Postgres.Name]; service.Type == "docker" {
		servers[service.Server] = true
		if len(app.Servers) > 1 || !contains(app.Servers, service.Server) {
			if err := requireInternalAddresses(servers, config); err != nil {
				return fmt.Errorf("apps.%s PostgreSQL: %w", appID, err)
			}
		}
	}
	servers = map[string]bool{}
	for _, server := range app.Servers {
		servers[server] = true
	}
	if service := config.Redis[app.Redis.Name]; service.Type == "docker" {
		servers[service.Server] = true
		if len(app.Servers) > 1 || !contains(app.Servers, service.Server) {
			if err := requireInternalAddresses(servers, config); err != nil {
				return fmt.Errorf("apps.%s Redis: %w", appID, err)
			}
		}
	}
	if app.OutboundProxy != nil && app.OutboundProxy.Enabled {
		servers = map[string]bool{}
		for _, server := range app.Servers {
			servers[server] = true
		}
		for _, server := range app.OutboundProxy.Servers {
			servers[server] = true
		}
		if len(servers) > 1 {
			if err := requireInternalAddresses(servers, config); err != nil {
				return fmt.Errorf("apps.%s MicroSocks: %w", appID, err)
			}
		}
	}
	return nil
}

func requireInternalAddresses(servers map[string]bool, config Config) error {
	for server := range servers {
		if config.Servers[server].Addresses == nil || config.Servers[server].Addresses.Internal == nil || !hasAddress(*config.Servers[server].Addresses.Internal) {
			return fmt.Errorf("server %q requires an internal address for cross-server connectivity", server)
		}
	}
	return nil
}
func validateSecrets(config Config, secrets Secrets) error {
	if err := rejectUnknown(secrets.Apps, ids(config.Apps), "apps"); err != nil {
		return err
	}
	if err := rejectUnknown(secrets.Postgres, ids(config.Postgres), "postgres"); err != nil {
		return err
	}
	if err := rejectUnknown(secrets.Redis, ids(config.Redis), "redis"); err != nil {
		return err
	}
	if err := rejectUnknown(secrets.OutboundProxy, ids(config.Servers), "outboundProxy"); err != nil {
		return err
	}
	for id, app := range config.Apps {
		secret, ok := secrets.Apps[id]
		if !ok {
			return fmt.Errorf("apps.%s secrets are required", id)
		}
		for field, value := range map[string]string{"initialAdminPassword": secret.InitialAdminPassword, "jwtSecret": secret.JWTSecret, "totpEncryptionKey": secret.TOTPEncryptionKey} {
			if value == "" {
				return fmt.Errorf("apps.%s.%s is required", id, field)
			}
		}
		if err := validateEnv("apps."+id+".environment", secret.Environment, app.Environment); err != nil {
			return err
		}
		if app.OutboundProxy != nil && app.OutboundProxy.Enabled {
			if secret.AdminAPIKey == "" {
				return fmt.Errorf("apps.%s.adminApiKey is required when outboundProxy is enabled", id)
			}
			for _, serverID := range app.OutboundProxy.Servers {
				proxySecret, exists := secrets.OutboundProxy[serverID]
				if !exists || proxySecret.Username == "" || proxySecret.Password == "" {
					return fmt.Errorf("outboundProxy.%s credentials are required", serverID)
				}
			}
		}
		pg := config.Postgres[app.Postgres.Name]
		if pg.Type != "neon" {
			if secret.Postgres == nil || secret.Postgres.Username == "" || secret.Postgres.Password == "" {
				return fmt.Errorf("apps.%s.postgres credentials are required", id)
			}
		} else if secret.postgresSet {
			return fmt.Errorf("apps.%s.postgres is forbidden for neon", id)
		}
		redis := config.Redis[app.Redis.Name]
		if redis.Type != "upstash" {
			if secret.Redis == nil || secret.Redis.Username == "" || secret.Redis.Password == "" {
				return fmt.Errorf("apps.%s.redis credentials are required", id)
			}
		} else if secret.redisSet {
			return fmt.Errorf("apps.%s.redis is forbidden for upstash", id)
		}
	}
	for id, service := range config.Postgres {
		secret, ok := secrets.Postgres[id]
		if service.Type == "external" {
			if ok && (secret.adminPasswordSet || secret.apiTokenSet) {
				return fmt.Errorf("postgres.%s service secrets are forbidden for external", id)
			}
			continue
		}
		if !ok {
			return fmt.Errorf("postgres.%s secrets are required", id)
		}
		if service.Type == "docker" && (!secret.adminPasswordSet || secret.AdminPassword == "") {
			return fmt.Errorf("postgres.%s.adminPassword is required", id)
		}
		if service.Type == "docker" && secret.apiTokenSet {
			return fmt.Errorf("postgres.%s.apiToken is only valid for neon", id)
		}
		if service.Type == "neon" && secret.adminPasswordSet {
			return fmt.Errorf("postgres.%s.adminPassword is only valid for docker", id)
		}
		if service.Type == "neon" && (!secret.apiTokenSet || secret.APIToken == "") {
			return fmt.Errorf("postgres.%s.apiToken is required for neon", id)
		}
	}
	for id, service := range config.Redis {
		secret, ok := secrets.Redis[id]
		if service.Type == "external" {
			if ok && (secret.adminPasswordSet || secret.apiKeySet) {
				return fmt.Errorf("redis.%s service secrets are forbidden for external", id)
			}
			continue
		}
		if !ok {
			return fmt.Errorf("redis.%s secrets are required", id)
		}
		if service.Type == "docker" && (!secret.adminPasswordSet || secret.AdminPassword == "") {
			return fmt.Errorf("redis.%s.adminPassword is required", id)
		}
		if service.Type == "docker" && secret.apiKeySet {
			return fmt.Errorf("redis.%s.apiKey is only valid for upstash", id)
		}
		if service.Type == "upstash" && secret.adminPasswordSet {
			return fmt.Errorf("redis.%s.adminPassword is only valid for docker", id)
		}
		if service.Type == "upstash" && (!secret.apiKeySet || secret.APIKey == "") {
			return fmt.Errorf("redis.%s.apiKey is required for upstash", id)
		}
	}
	for id, proxy := range secrets.OutboundProxy {
		if proxy.Password == "" {
			return fmt.Errorf("outboundProxy.%s.password is required", id)
		}
	}
	return nil
}

func validateEnv(name string, values map[string]string, ordinary map[string]string) error {
	for key, value := range values {
		if !envKeyPattern.MatchString(key) || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%s contains an invalid environment entry", name)
		}
		if _, ok := ordinary[key]; ok {
			return fmt.Errorf("%s overlaps ordinary environment key %q", name, key)
		}
		if reservedEnvKey(key) {
			return fmt.Errorf("%s contains deployment-owned environment key %q", name, key)
		}
	}
	return nil
}
func reservedEnvKey(key string) bool {
	for _, prefix := range []string{"DATABASE_", "POSTGRES_", "REDIS_", "SITE_", "COMPOSE_", "TRAEFIK_", "CLOUDFLARE_"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	for _, value := range []string{
		"SITE_ID", "SITE_RUNTIME_ROOT", "SITE_RUNTIME_ENV_PATH", "SITE_APP_ENV_PATH", "SITE_DEPLOY_STATE_PATH", "SITE_BOOTSTRAP_MARKER_PATH",
		"COMPOSE_PROJECT_NAME", "SITE_ROUTE_PATH", "BLUE_DATA_PATH", "GREEN_DATA_PATH", "BLUE_EDGE_ALIAS", "GREEN_EDGE_ALIAS",
		"ACTIVE_EDGE_ALIAS", "EDGE_NETWORK_NAME", "DOMAIN", "ORIGIN_IP", "APP_PROBE_PATH", "DRAIN_SECONDS", "CONFIGURED_SITE_IDS",
		"HOST_STATE_PATH", "SUB2API_IMAGE", "SLOT", "SLOT_DATA_DIR", "AUTO_SETUP", "RUNTIME_ROOT", "ACTIVE_SLOT", "APP_ENV_CONFIGURED",
		"APP_ENV_JSON", "RUNTIME_JSON", "TRAEFIK_IMAGE", "CLOUDFLARE_DNS_API_TOKEN", "CLOUDFLARE_API_TOKEN", "ACME_EMAIL",
		"SING_BOX_SERVER_NAME", "SING_BOX_TARGET", "SING_BOX_CONFIG", "EDGE_RUNTIME_ROOT", "POSTGRES_MODE", "REDIS_MODE",
		"PROBE_RETRIES", "PROBE_DELAY_SECONDS", "BIND_HOST", "SERVER_HOST", "SERVER_PORT", "SERVER_MODE", "RUN_MODE",
		"ADMIN_EMAIL", "ADMIN_PASSWORD", "JWT_SECRET", "TOTP_ENCRYPTION_KEY", "PGDATA", "REDISCLI_AUTH", "IMAGE", "PATH", "PROBE",
	} {
		if key == value {
			return true
		}
	}
	return false
}
func rejectUnknown[T any](values map[string]T, allowedIDs map[string]struct{}, name string) error {
	for id := range values {
		if _, ok := allowedIDs[id]; !ok {
			return fmt.Errorf("%s.%s is not configured", name, id)
		}
	}
	return nil
}

func ids[T any](values map[string]T) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for id := range values {
		result[id] = struct{}{}
	}
	return result
}
func validateHostPort(value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("must be host:port")
	}
	if net.ParseIP(host) == nil && (host != strings.ToLower(host) || !validHostname(host)) {
		return fmt.Errorf("must contain a valid host")
	}
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(port) {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}
func hasAddress(set AddressSet) bool { return set.IPv4 != "" || set.IPv6 != "" }
func validHostname(value string) bool {
	return len(value) <= 253 && hostPattern.MatchString(value)
}
func hasPublicAddress(server Server) bool {
	return server.Addresses != nil && server.Addresses.Public != nil && hasAddress(*server.Addresses.Public)
}
func unique(values []string) bool {
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}
func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
func ResolveEnvironment(workdir, environment string) (EnvironmentPaths, error) {
	if !idPattern.MatchString(environment) {
		return EnvironmentPaths{}, fmt.Errorf("environment ID %q is invalid", environment)
	}
	environmentsRoot, err := filepath.EvalSymlinks(filepath.Join(workdir, "environments"))
	if err != nil {
		return EnvironmentPaths{}, fmt.Errorf("resolve environments root: %w", err)
	}
	rootInfo, err := os.Stat(environmentsRoot)
	if err != nil || !rootInfo.IsDir() {
		return EnvironmentPaths{}, fmt.Errorf("environments root is not a directory")
	}
	directory := filepath.Join(environmentsRoot, environment)
	resolvedDirectory, err := filepath.EvalSymlinks(directory)
	if err != nil || !within(environmentsRoot, resolvedDirectory) {
		return EnvironmentPaths{}, fmt.Errorf("environment directory escapes environments root")
	}
	configPath, err := resolveContainedFile(environmentsRoot, filepath.Join(resolvedDirectory, "config.yaml"))
	if err != nil {
		return EnvironmentPaths{}, err
	}
	secretsPath, err := resolveContainedFile(environmentsRoot, filepath.Join(resolvedDirectory, "secrets.yaml"))
	if err != nil {
		return EnvironmentPaths{}, err
	}
	return EnvironmentPaths{Root: environmentsRoot, Directory: resolvedDirectory, Config: configPath, Secrets: secretsPath}, nil
}
func resolveContainedFile(root, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve environment file: %w", err)
	}
	if !within(root, resolved) {
		return "", fmt.Errorf("environment file escapes environments root")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("environment file is not regular")
	}
	return resolved, nil
}
func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && relative != "."
}
