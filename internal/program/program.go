package program

import (
	"fmt"
	"net"
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

	managedRedis := make(map[string]*upstash.RedisDatabase)
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
		managedRedis[id] = database
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

	hosts := make(map[string]*hostResource, len(validated.ServerIDs))
	for _, serverID := range validated.ServerIDs {
		var host hostResource
		var options []pulumi.ResourceOption
		if dependencies := appDependencies(serverID, validated.Config, hosts); len(dependencies) != 0 {
			options = append(options, pulumi.DependsOn(dependencies))
		}
		err := ctx.RegisterResource(hostresource.HostToken, "host-"+serverID, pulumi.Map{
			"resource": pulumi.Map{
				"environment": pulumi.String(ctx.Stack()),
				"serverKey":   pulumi.String(serverID),
			},
			"server": pulumi.Map{"sshAlias": pulumi.String(validated.Servers[serverID].SSHAlias)},
			"target": hostTarget(validated.Config, releaseArtifact, serverID, managedRedis),
			"secrets": hostSecrets(validated.Config, secrets, serverID, managedRedis),
		}, &host, options...)
		if err != nil {
			return err
		}
		hosts[serverID] = &host
	}

	if cloudflareProvider == nil {
		return nil
	}
	for _, appID := range sortedAppIDs(validated.Config) {
		app := validated.Apps[appID]
		if app.PublicAccess.Type != "cloudflare" {
			continue
		}
		for _, serverID := range sortedStrings(app.PublicAccess.Servers) {
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
				}, pulumi.Provider(cloudflareProvider), pulumi.DependsOn([]pulumi.Resource{hosts[serverID]}))
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
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

func hostTarget(config environment.Config, release, server string, managed map[string]*upstash.RedisDatabase) pulumi.Map {
	apps := pulumi.Array{}
	for _, id := range sortedAppIDs(config) {
		app := config.Apps[id]
		if !contains(app.Servers, server) {
			continue
		}
		servers := sortedStrings(app.Servers)
		links := pulumi.Array{postgresLink(app.Postgres.Name, app.Postgres.Database, config.Postgres[app.Postgres.Name])}
		redis := config.Redis[app.Redis.Name]
		if redis.Type == "upstash" {
			links = append(links, managedRedisLink(app.Redis.Name, fmt.Sprint(app.Redis.Database), managed[app.Redis.Name]))
		} else {
			links = append(links, redisLink(app.Redis.Name, fmt.Sprint(app.Redis.Database), redis))
		}
		settings := copyStringMap(app.Environment)
		settings["ADMIN_EMAIL"] = app.InitialAdminEmail
		apps = append(apps, pulumi.Map{
			"id":               pulumi.String(id),
			"image":            pulumi.String(app.Image),
			"hostname":         pulumi.String(app.Hostname),
			"readinessPath":    pulumi.String(app.ReadinessPath),
			"drainTimeout":     pulumi.String(app.DrainTimeout.String()),
			"initialBootstrap": pulumi.Bool(server == servers[0]),
			"runtimeSettings":  stringMapInput(settings),
			"dataLinks":        links,
		})
	}
	services := localServices(config, server)
	return pulumi.Map{
		"releaseArtifact": pulumi.String(release),
		"apps":            apps,
		"dataServices":    services,
		"reverseProxy": pulumi.Map{
			"image":     pulumi.String(config.ReverseProxy.Image),
			"acmeEmail": pulumi.String(config.ReverseProxy.AcmeEmail),
		},
	}
}

func postgresLink(id, database string, service environment.Postgres) pulumi.Map {
	endpoint := service.Host
	if service.Type == "docker" {
		endpoint = "postgres"
	}
	return pulumi.Map{"name": pulumi.String(id), "identity": pulumi.Map{
		"kind":          pulumi.String("postgres"),
		"providerId":    pulumi.String(id),
		"endpoint":      pulumi.String(endpoint),
		"port":          pulumi.Int(*service.Port),
		"database":      pulumi.String(database),
		"tlsServerName": pulumi.String(endpoint),
	}}
}

func redisLink(id, database string, service environment.Redis) pulumi.Map {
	endpoint := service.Host
	if service.Type == "docker" {
		endpoint = "redis"
	}
	return pulumi.Map{"name": pulumi.String(id), "identity": pulumi.Map{
		"kind":       pulumi.String("redis"),
		"providerId": pulumi.String(id),
		"endpoint":   pulumi.String(endpoint),
		"port":       pulumi.Int(*service.Port),
		"database":   pulumi.String(database),
	}}
}

func managedRedisLink(id, database string, service *upstash.RedisDatabase) pulumi.Map {
	return pulumi.Map{"name": pulumi.String(id), "identity": pulumi.Map{
		"kind":       pulumi.String("redis"),
		"providerId": service.ID(),
		"endpoint":   service.Endpoint,
		"port":       service.Port,
		"database":   pulumi.String(database),
	}}
}

func localServices(config environment.Config, server string) pulumi.Array {
	services := pulumi.Array{}
	for _, id := range sortedPostgresIDs(config) {
		service := config.Postgres[id]
		if service.Type == "docker" && service.Server == server {
			services = append(services, pulumi.Map{"id": pulumi.String(id), "type": pulumi.String("postgres"), "port": pulumi.Int(*service.Port)})
		}
	}
	for _, id := range sortedRedisIDs(config) {
		service := config.Redis[id]
		if service.Type == "docker" && service.Server == server {
			services = append(services, pulumi.Map{"id": pulumi.String(id), "type": pulumi.String("redis"), "port": pulumi.Int(*service.Port), "persistence": pulumi.Bool(*service.Persistence)})
		}
	}
	return services
}

func hostSecrets(config environment.Config, secrets environment.Secrets, server string, managed map[string]*upstash.RedisDatabase) pulumi.Input {
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

func stringMapInput(values map[string]string) pulumi.Map {
	result := pulumi.Map{}
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
