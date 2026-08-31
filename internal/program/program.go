package program

import (
	"fmt"
	"net"
	"reflect"
	"regexp"
	"sort"
	"time"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/environment"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostresource"
	"github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/upstash/pulumi-upstash/sdk/go/upstash"
)

var releaseArtifactPattern = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

type hostResource struct{ pulumi.CustomResourceState }

type managedRedisInputs struct {
	ProviderID pulumi.StringInput
	Endpoint   pulumi.StringInput
	Port       pulumi.IntInput
	Password   pulumi.StringInput
}

type hostRegistrationValue struct {
	Resource map[string]any  `pulumi:"resource"`
	Server   map[string]any  `pulumi:"server"`
	Target   hostTargetValue `pulumi:"target"`
	Secrets  map[string]any  `pulumi:"secrets"`
}

type hostRegistrationInputs struct {
	Resource pulumi.MapInput  `pulumi:"resource"`
	Server   pulumi.MapInput  `pulumi:"server"`
	Target   hostTargetInputs `pulumi:"target"`
	Secrets  pulumi.Input     `pulumi:"secrets"`
}

func (hostRegistrationInputs) ElementType() reflect.Type {
	return reflect.TypeOf(hostRegistrationValue{})
}

type hostTargetValue struct {
	ReleaseArtifact string                  `pulumi:"releaseArtifact"`
	Apps            []hostAppValue          `pulumi:"apps"`
	DataServices    []localDataServiceValue `pulumi:"dataServices"`
	ReverseProxy    reverseProxyValue       `pulumi:"reverseProxy"`
}

type hostTargetInputs struct {
	ReleaseArtifact string                    `pulumi:"releaseArtifact"`
	Apps            []hostAppInputs          `pulumi:"apps"`
	DataServices    []localDataServiceInputs `pulumi:"dataServices"`
	ReverseProxy    reverseProxyInputs      `pulumi:"reverseProxy"`
}

func (hostTargetInputs) ElementType() reflect.Type {
	return reflect.TypeOf(hostTargetValue{})
}

type hostAppValue struct {
	ID               string            `pulumi:"id"`
	Image            string            `pulumi:"image"`
	Hostname         string            `pulumi:"hostname"`
	ReadinessPath    string            `pulumi:"readinessPath"`
	DrainTimeout     string            `pulumi:"drainTimeout"`
	InitialBootstrap bool              `pulumi:"initialBootstrap"`
	RuntimeSettings  map[string]string `pulumi:"runtimeSettings"`
	DataLinks        []dataLinkValue   `pulumi:"dataLinks"`
}

type hostAppInputs struct {
	ID               string                `pulumi:"id"`
	Image            string                `pulumi:"image"`
	Hostname         string                `pulumi:"hostname"`
	ReadinessPath    string                `pulumi:"readinessPath"`
	DrainTimeout     string                `pulumi:"drainTimeout"`
	InitialBootstrap bool                  `pulumi:"initialBootstrap"`
	RuntimeSettings  pulumi.StringMapInput `pulumi:"runtimeSettings"`
	DataLinks        []dataLinkInputs      `pulumi:"dataLinks"`
}

func (hostAppInputs) ElementType() reflect.Type {
	return reflect.TypeOf(hostAppValue{})
}

type dataLinkValue struct {
	Name     string           `pulumi:"name"`
	Identity dataIdentityValue `pulumi:"identity"`
}

type dataLinkInputs struct {
	Name     string              `pulumi:"name"`
	Identity dataIdentityInputs `pulumi:"identity"`
}

func (dataLinkInputs) ElementType() reflect.Type {
	return reflect.TypeOf(dataLinkValue{})
}

type dataIdentityValue struct {
	Kind          string `pulumi:"kind"`
	ProviderID    string `pulumi:"providerId"`
	Endpoint      string `pulumi:"endpoint"`
	Port          int    `pulumi:"port"`
	Database      string `pulumi:"database"`
	TLSServerName string `pulumi:"tlsServerName,optional"`
}

type dataIdentityInputs struct {
	Kind          string             `pulumi:"kind"`
	ProviderID    pulumi.StringInput `pulumi:"providerId"`
	Endpoint      pulumi.StringInput `pulumi:"endpoint"`
	Port          pulumi.IntInput    `pulumi:"port"`
	Database      string             `pulumi:"database"`
	TLSServerName pulumi.StringInput `pulumi:"tlsServerName,optional"`
}

func (dataIdentityInputs) ElementType() reflect.Type {
	return reflect.TypeOf(dataIdentityValue{})
}

type localDataServiceValue struct {
	ID          string `pulumi:"id"`
	Type        string `pulumi:"type"`
	Port        int    `pulumi:"port"`
	Persistence bool   `pulumi:"persistence,optional"`
}

type localDataServiceInputs struct {
	ID          pulumi.StringInput `pulumi:"id"`
	Type        pulumi.StringInput `pulumi:"type"`
	Port        pulumi.IntInput    `pulumi:"port"`
	Persistence pulumi.BoolInput   `pulumi:"persistence,optional"`
}

func (localDataServiceInputs) ElementType() reflect.Type {
	return reflect.TypeOf(localDataServiceValue{})
}

type reverseProxyValue struct {
	Image     string `pulumi:"image"`
	ACMEEmail string `pulumi:"acmeEmail"`
}

type reverseProxyInputs struct {
	Image     string `pulumi:"image"`
	ACMEEmail string `pulumi:"acmeEmail"`
}

func (reverseProxyInputs) ElementType() reflect.Type {
	return reflect.TypeOf(reverseProxyValue{})
}

func Register(ctx *pulumi.Context, releaseArtifact string, configYAML, secretsYAML []byte) error {
	config, err := environment.ParseConfig(configYAML)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	secrets, err := environment.ParseSecrets(secretsYAML)
	if err != nil {
		return fmt.Errorf("parse secrets: %w", err)
	}
	validated, err := environment.Validate(config, secrets)
	if err != nil {
		return fmt.Errorf("validate environment: %w", err)
	}
	if !releaseArtifactPattern.MatchString(releaseArtifact) {
		return fmt.Errorf("release artifact must be a nonempty immutable sha256 image reference")
	}
	if err := preflight(validated); err != nil {
		return err
	}

	managedRedis := make(map[string]managedRedisInputs)
	for _, id := range sortedRedisIDs(validated.Config) {
		service := validated.Redis[id]
		if service.Type != "upstash" {
			continue
		}
		provider, err := upstash.NewProvider(ctx, "upstash-"+id, &upstash.ProviderArgs{
			ApiKey: secretStringPtr(secrets.Redis[id].APIKey),
		})
		if err != nil {
			return err
		}
		database, err := upstash.NewRedisDatabase(ctx, id, &upstash.RedisDatabaseArgs{
			DatabaseName: pulumi.String(id),
			Region:       pulumi.String(service.Region),
			Tls:          pulumi.BoolPtr(true),
		}, pulumi.Provider(provider), pulumi.Protect(true), pulumi.RetainOnDelete(true))
		if err != nil {
			return err
		}
		managedRedis[id] = managedRedisInputs{
			ProviderID: database.ID().ToStringOutput(),
			Endpoint:   database.Endpoint,
			Port:       database.Port,
			Password:   pulumi.ToSecret(database.Password).(pulumi.StringOutput),
		}
	}

	var cloudflareProvider *cloudflare.Provider
	if hasCloudflare(validated.Config) {
		cloudflareProvider, err = cloudflare.NewProvider(ctx, "cloudflare", &cloudflare.ProviderArgs{
			ApiToken: secretStringPtr(secrets.Cloudflare.APIToken),
		})
		if err != nil {
			return err
		}
	}

	hosts, err := registerHosts(ctx, validated, secrets, releaseArtifact, managedRedis)
	if err != nil {
		return err
	}

	if cloudflareProvider == nil {
		return nil
	}
	for _, appID := range sortedAppIDs(validated.Config) {
		app := validated.Apps[appID]
		if app.PublicAccess.Type != "cloudflare" {
			continue
		}
		publicServers := sortedStrings(app.PublicAccess.Servers)
		publicHostDependencies := make([]pulumi.Resource, 0, len(publicServers))
		for _, serverID := range publicServers {
			publicHostDependencies = append(publicHostDependencies, hosts[serverID])
		}
		for _, serverID := range publicServers {
			for _, address := range publicAddresses(validated.Servers[serverID]) {
				recordType := "A"
				if net.ParseIP(address).To4() == nil {
					recordType = "AAAA"
				}
				_, err := cloudflare.NewDnsRecord(ctx, "dns-"+appID+"-"+serverID+"-"+recordType, &cloudflare.DnsRecordArgs{
					Name:    pulumi.String(app.Hostname),
					Content: pulumi.StringPtr(address),
					Proxied: pulumi.BoolPtr(true),
					Ttl:     pulumi.Float64(1),
					Type:    pulumi.String(recordType),
					ZoneId:  pulumi.String(validated.Cloudflare.ZoneID),
				}, pulumi.Provider(cloudflareProvider), pulumi.DependsOn(publicHostDependencies))
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func registerHosts(ctx *pulumi.Context, validated environment.ValidatedConfig, secrets environment.Secrets, release string, managed map[string]managedRedisInputs) (map[string]*hostResource, error) {
	hosts := make(map[string]*hostResource, len(validated.ServerIDs))
	for _, serverID := range validated.ServerIDs {
		var host hostResource
		var options []pulumi.ResourceOption
		if dependencies := appDependencies(serverID, validated.Config, hosts); len(dependencies) != 0 {
			options = append(options, pulumi.DependsOn(dependencies))
		}
		err := ctx.RegisterResource(hostresource.HostToken, "host-"+serverID, hostRegistrationInputs{
			Resource: pulumi.Map{
				"environment": pulumi.String(ctx.Stack()),
				"serverKey":   pulumi.String(serverID),
			},
			Server:  pulumi.Map{"sshAlias": pulumi.String(validated.Servers[serverID].SSHAlias)},
			Target:  hostTarget(validated.Config, release, serverID, managed),
			Secrets: hostSecrets(validated.Config, secrets, serverID, managed),
		}, &host, options...)
		if err != nil {
			return nil, err
		}
		hosts[serverID] = &host
	}
	return hosts, nil
}

func preflight(config environment.ValidatedConfig) error {
	hostnames := map[string]string{}
	for id := range config.Postgres {
		if _, exists := config.Redis[id]; exists {
			return fmt.Errorf("data service ID %q is used by both PostgreSQL and Redis", id)
		}
	}
	for _, server := range config.Servers {
		if server.SingBox != nil {
			return fmt.Errorf("SingBox is unsupported before first registration")
		}
	}
	for _, service := range config.Postgres {
		if service.Type == "neon" {
			return fmt.Errorf("managed Neon is unsupported before first registration")
		}
		if service.Type == "external" && service.TLS != nil && service.TLS.Mode == "disable" {
			return fmt.Errorf("external PostgreSQL TLS disable is unsupported before first registration")
		}
	}
	for id, app := range config.Apps {
		if other, exists := hostnames[app.Hostname]; exists {
			return fmt.Errorf("apps.%s.hostname duplicates apps.%s.hostname", id, other)
		}
		hostnames[app.Hostname] = id
		if app.DrainTimeout.Duration < time.Second || app.DrainTimeout.Duration > 10*time.Minute || app.DrainTimeout.Duration%time.Second != 0 {
			return fmt.Errorf("apps.%s.drainTimeout must be whole seconds between 1s and 10m", id)
		}
		if app.OutboundProxy != nil && app.OutboundProxy.Enabled {
			return fmt.Errorf("apps.%s outboundProxy is unsupported before first registration", id)
		}
		if app.PublicAccess.Type == "cloudflare" && app.PublicAccess.Cloudflare.Mode != "dns" {
			return fmt.Errorf("Cloudflare public access mode %q is unsupported before first registration", app.PublicAccess.Cloudflare.Mode)
		}
		if err := rejectCrossHostDocker(id, "PostgreSQL", config.Postgres[app.Postgres.Name].Type, config.Postgres[app.Postgres.Name].Server, app.Servers); err != nil {
			return err
		}
		if err := rejectCrossHostDocker(id, "Redis", config.Redis[app.Redis.Name].Type, config.Redis[app.Redis.Name].Server, app.Servers); err != nil {
			return err
		}
	}
	return nil
}

func rejectCrossHostDocker(appID, kind, serviceType, serviceServer string, appServers []string) error {
	if serviceType != "docker" {
		return nil
	}
	for _, server := range appServers {
		if server != serviceServer {
			return fmt.Errorf("apps.%s %s Docker service is on a different server", appID, kind)
		}
	}
	return nil
}

func hostTarget(config environment.Config, release, server string, managed map[string]managedRedisInputs) hostTargetInputs {
	apps := []hostAppInputs{}
	for _, id := range sortedAppIDs(config) {
		app := config.Apps[id]
		if !contains(app.Servers, server) {
			continue
		}
		servers := sortedStrings(app.Servers)
		links := []dataLinkInputs{postgresLink(app.Postgres.Name, app.Postgres.Database, config.Postgres[app.Postgres.Name])}
		redis := config.Redis[app.Redis.Name]
		if redis.Type == "upstash" {
			links = append(links, managedRedisLink(app.Redis.Name, fmt.Sprint(app.Redis.Database), managed[app.Redis.Name]))
		} else {
			links = append(links, redisLink(app.Redis.Name, fmt.Sprint(app.Redis.Database), redis))
		}
		settings := copyStringMap(app.Environment)
		settings["ADMIN_EMAIL"] = app.InitialAdminEmail
		apps = append(apps, hostAppInputs{
			ID:               id,
			Image:            app.Image,
			Hostname:         app.Hostname,
			ReadinessPath:    app.ReadinessPath,
			DrainTimeout:     app.DrainTimeout.String(),
			InitialBootstrap: server == servers[0],
			RuntimeSettings:  stringMapInput(settings),
			DataLinks:        links,
		})
	}
	return hostTargetInputs{
		ReleaseArtifact: release,
		Apps:            apps,
		DataServices:    localServices(config, server),
		ReverseProxy: reverseProxyInputs{
			Image:     config.ReverseProxy.Image,
			ACMEEmail: config.ReverseProxy.AcmeEmail,
		},
	}
}

func postgresLink(id, database string, service environment.Postgres) dataLinkInputs {
	endpoint := service.Host
	if service.Type == "docker" {
		endpoint = "postgres"
	}
	return dataLinkInputs{Name: id, Identity: dataIdentityInputs{
		Kind:          "postgres",
		ProviderID:    pulumi.String(id),
		Endpoint:      pulumi.String(endpoint),
		Port:          pulumi.Int(*service.Port),
		Database:      database,
		TLSServerName: pulumi.String(endpoint),
	}}
}

func redisLink(id, database string, service environment.Redis) dataLinkInputs {
	endpoint := service.Host
	if service.Type == "docker" {
		endpoint = "redis"
	}
	return dataLinkInputs{Name: id, Identity: dataIdentityInputs{
		Kind:       "redis",
		ProviderID: pulumi.String(id),
		Endpoint:   pulumi.String(endpoint),
		Port:       pulumi.Int(*service.Port),
		Database:   database,
	}}
}

func managedRedisLink(id, database string, service managedRedisInputs) dataLinkInputs {
	return dataLinkInputs{
		Name: id,
		Identity: dataIdentityInputs{
			Kind:       "redis",
			ProviderID: service.ProviderID,
			Endpoint:   service.Endpoint,
			Port:       service.Port,
			Database:   database,
		},
	}
}

func localServices(config environment.Config, server string) []localDataServiceInputs {
	services := []localDataServiceInputs{}
	for _, id := range sortedPostgresIDs(config) {
		service := config.Postgres[id]
		if service.Type == "docker" && service.Server == server {
			services = append(services, localDataServiceInputs{ID: pulumi.String(id), Type: pulumi.String("postgres"), Port: pulumi.Int(*service.Port)})
		}
	}
	for _, id := range sortedRedisIDs(config) {
		service := config.Redis[id]
		if service.Type == "docker" && service.Server == server {
			services = append(services, localDataServiceInputs{ID: pulumi.String(id), Type: pulumi.String("redis"), Port: pulumi.Int(*service.Port), Persistence: pulumi.Bool(*service.Persistence)})
		}
	}
	return services
}

func hostSecrets(config environment.Config, secrets environment.Secrets, server string, managed map[string]managedRedisInputs) pulumi.Input {
	apps := pulumi.Map{}
	for _, id := range sortedAppIDs(config) {
		app := config.Apps[id]
		if !contains(app.Servers, server) {
			continue
		}
		appSecret := secrets.Apps[id]
		redis := config.Redis[app.Redis.Name]
		var redisUsername string
		var redisPassword pulumi.Input
		if redis.Type == "upstash" {
			redisUsername = "default"
			redisPassword = managed[app.Redis.Name].Password
		} else {
			redisUsername = appSecret.Redis.Username
			redisPassword = pulumi.String(appSecret.Redis.Password)
		}
		value := pulumi.Map{
			"jwtSecret":         pulumi.String(appSecret.JWTSecret),
			"totpEncryptionKey": pulumi.String(appSecret.TOTPEncryptionKey),
			"runtimeEnvironment": stringMapInput(copyStringMap(appSecret.Environment)),
			"postgres": pulumi.Map{
				"username": pulumi.String(appSecret.Postgres.Username),
				"password": pulumi.String(appSecret.Postgres.Password),
			},
			"redis": pulumi.Map{
				"username": pulumi.String(redisUsername),
				"password": redisPassword,
			},
		}
		if server == sortedStrings(app.Servers)[0] {
			value["initialAdminPassword"] = pulumi.String(appSecret.InitialAdminPassword)
		}
		if appSecret.AdminAPIKey != "" {
			value["adminApiKey"] = pulumi.String(appSecret.AdminAPIKey)
		}
		apps[id] = value
	}

	local := pulumi.Map{}
	for _, id := range sortedPostgresIDs(config) {
		service := config.Postgres[id]
		if service.Type == "docker" && service.Server == server {
			local[id] = pulumi.Map{"adminPassword": pulumi.String(secrets.Postgres[id].AdminPassword)}
		}
	}
	for _, id := range sortedRedisIDs(config) {
		service := config.Redis[id]
		if service.Type == "docker" && service.Server == server {
			local[id] = pulumi.Map{"adminPassword": pulumi.String(secrets.Redis[id].AdminPassword)}
		}
	}
	return pulumi.ToSecret(pulumi.Map{
		"reverseProxy":      pulumi.Map{"dnsChallengeToken": pulumi.String(secrets.ReverseProxy.DNSChallengeToken)},
		"apps":              apps,
		"localDataServices": local,
	})
}

func appDependencies(server string, config environment.Config, hosts map[string]*hostResource) []pulumi.Resource {
	dependencies := []pulumi.Resource{}
	seen := map[string]bool{}
	for _, id := range sortedAppIDs(config) {
		servers := sortedStrings(config.Apps[id].Servers)
		for index := 1; index < len(servers); index++ {
			if servers[index] != server {
				continue
			}
			previous := servers[index-1]
			if !seen[previous] {
				dependencies = append(dependencies, hosts[previous])
				seen[previous] = true
			}
		}
	}
	return dependencies
}

func secretStringPtr(value string) pulumi.StringPtrInput {
	return pulumi.ToSecret(pulumi.StringPtr(value)).(pulumi.StringPtrInput)
}

func stringMapInput(values map[string]string) pulumi.StringMap {
	result := pulumi.StringMap{}
	for key, value := range values {
		result[key] = pulumi.String(value)
	}
	return result
}

func copyStringMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func hasCloudflare(config environment.Config) bool {
	for _, app := range config.Apps {
		if app.PublicAccess.Type == "cloudflare" {
			return true
		}
	}
	return false
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func sortedAppIDs(config environment.Config) []string {
	result := make([]string, 0, len(config.Apps))
	for id := range config.Apps {
		result = append(result, id)
	}
	return sortedStrings(result)
}

func sortedPostgresIDs(config environment.Config) []string {
	result := make([]string, 0, len(config.Postgres))
	for id := range config.Postgres {
		result = append(result, id)
	}
	return sortedStrings(result)
}

func sortedRedisIDs(config environment.Config) []string {
	result := make([]string, 0, len(config.Redis))
	for id := range config.Redis {
		result = append(result, id)
	}
	return sortedStrings(result)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func publicAddresses(server environment.Server) []string {
	if server.Addresses == nil || server.Addresses.Public == nil {
		return nil
	}
	addresses := []string{}
	if server.Addresses.Public.IPv4 != "" {
		addresses = append(addresses, server.Addresses.Public.IPv4)
	}
	if server.Addresses.Public.IPv6 != "" {
		addresses = append(addresses, server.Addresses.Public.IPv6)
	}
	return addresses
}
