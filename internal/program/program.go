package program

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/environment"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostcontract"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostimport"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostresource"
	"github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"
	"github.com/upstash/pulumi-upstash/sdk/go/upstash"
)

var releaseArtifactPattern = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

const importTargetConfigKey = "sub2api-environment:hostImportTarget"

type hostResource struct{ pulumi.CustomResourceState }

type managedRedisInputs struct {
	ProviderID pulumi.StringInput
	Endpoint   pulumi.StringInput
	Port       pulumi.IntInput
	Password   pulumi.StringInput
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
	if err := preflight(validated, secrets); err != nil {
		return err
	}
	importTarget, key, err := importPreflight(ctx, validated, secrets)
	if err != nil {
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

	hosts, err := registerHosts(ctx, validated, secrets, releaseArtifact, managedRedis, importTarget, key)
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

func importPreflight(ctx *pulumi.Context, validated environment.ValidatedConfig, secrets environment.Secrets) (string, hostcontract.RevisionKey, error) {
	importTarget, _ := ctx.GetConfig(importTargetConfigKey)
	if importTarget == "" {
		return "", nil, nil
	}
	if !contains(validated.ServerIDs, importTarget) {
		return "", nil, fmt.Errorf("invalid Host import target")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(secrets.RevisionKey)
	if err != nil || len(decoded) != 32 {
		return "", nil, fmt.Errorf("invalid Host import target")
	}
	return importTarget, hostcontract.RevisionKey(decoded), nil
}

func registerHosts(ctx *pulumi.Context, validated environment.ValidatedConfig, secrets environment.Secrets, release string, managed map[string]managedRedisInputs, importTarget string, key hostcontract.RevisionKey) (map[string]*hostResource, error) {
	hosts := make(map[string]*hostResource, len(validated.ServerIDs))
	dockerDependencies := dockerDataOwnerDependencies(validated.Config)
	order, err := hostOrderWithDockerDataDependencies(validated.Config, dockerDependencies)
	if err != nil {
		return nil, err
	}
	for _, serverID := range order {
		var host hostResource
		var options []pulumi.ResourceOption
		if dependencies := hostDependencies(serverID, validated.Config, hosts, dockerDependencies); len(dependencies) != 0 {
			options = append(options, pulumi.DependsOn(dependencies))
		}
		inputs := pulumi.Map{
			"resource": pulumi.Map{
				"environment": pulumi.String(ctx.Stack()),
				"serverKey":   pulumi.String(serverID),
			},
			"server":  pulumi.Map{"sshAlias": pulumi.String(validated.Servers[serverID].SSHAlias)},
			"target":  hostTarget(validated.Config, secrets, release, serverID, managed),
			"secrets": hostSecrets(validated.Config, secrets, serverID, managed),
		}
		if serverID == importTarget {
			id, importErr := hostImportID(ctx, key, inputs)
			if importErr != nil {
				return nil, importErr
			}
			options = append(options, pulumi.Import(id))
		}
		err := ctx.RegisterResource(hostresource.HostToken, "host-"+serverID, inputs, &host, options...)
		if err != nil {
			return nil, err
		}
		hosts[serverID] = &host
	}
	return hosts, nil
}

func hostImportID(ctx *pulumi.Context, key hostcontract.RevisionKey, inputs pulumi.Map) (pulumi.ID, error) {
	id := pulumi.ToOutput(inputs).ApplyT(func(value any) (pulumi.ID, error) {
		payload, err := json.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("invalid Host import inputs")
		}
		var tokenInputs hostimport.Inputs
		if json.Unmarshal(payload, &tokenInputs) != nil {
			return "", fmt.Errorf("invalid Host import inputs")
		}
		token, err := hostimport.Encode(key, tokenInputs)
		if err != nil {
			return "", fmt.Errorf("invalid Host import inputs")
		}
		return pulumi.ID(token), nil
	}).(pulumi.IDOutput)
	// Import IDs are a synchronous SDK requirement. Do not await through the
	// engine context, which may itself be waiting for this registration.
	resolved, err := internals.UnsafeAwaitOutput(context.Background(), id)
	if err != nil || !resolved.Known {
		return "", fmt.Errorf("invalid Host import inputs")
	}
	value, ok := resolved.Value.(pulumi.ID)
	if !ok {
		return "", fmt.Errorf("invalid Host import inputs")
	}
	return value, nil
}

func preflight(config environment.ValidatedConfig, secrets environment.Secrets) error {
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
	}
	if err := validateDockerDataClients(config.Config, secrets); err != nil {
		return err
	}
	_, err := hostOrder(config.Config)
	return err
}

func validateDockerDataClients(config environment.Config, secrets environment.Secrets) error {
	type principal struct{ database, password string }
	principals := map[string]map[string]principal{}
	sockets := map[string]string{}
	for appID, app := range config.Apps {
		if secret := secrets.Apps[appID].Postgres; secret != nil && !validPostgresDSNPassword(secret.Password) {
			return fmt.Errorf("invalid PostgreSQL DSN password for app %q", appID)
		}
		postgresUser, postgresPassword, redisUser, redisPassword := "", "", "", ""
		if config.Postgres[app.Postgres.Name].Type == "docker" {
			if !validPostgresIdentifier(secrets.Apps[appID].Postgres.Username) || !validPostgresDatabase(app.Postgres.Database) {
				return fmt.Errorf("invalid Docker PostgreSQL client identity for app %q", appID)
			}
			postgresUser, postgresPassword = secrets.Apps[appID].Postgres.Username, secrets.Apps[appID].Postgres.Password
		}
		if config.Redis[app.Redis.Name].Type == "docker" {
			redisUser, redisPassword = secrets.Apps[appID].Redis.Username, secrets.Apps[appID].Redis.Password
		}
		for _, data := range []struct {
			id, owner, database, username, password string
			port                                    int
		}{
			{app.Postgres.Name, config.Postgres[app.Postgres.Name].Server, app.Postgres.Database, postgresUser, postgresPassword, portOrZero(config.Postgres[app.Postgres.Name].Port)},
			{app.Redis.Name, config.Redis[app.Redis.Name].Server, fmt.Sprint(app.Redis.Database), redisUser, redisPassword, portOrZero(config.Redis[app.Redis.Name].Port)},
		} {
			if data.owner == "" {
				continue
			}
			if principals[data.id] == nil {
				principals[data.id] = map[string]principal{}
			}
			if old, exists := principals[data.id][data.username]; exists && old != (principal{data.database, data.password}) {
				return fmt.Errorf("incompatible Docker data username %q", data.username)
			}
			principals[data.id][data.username] = principal{data.database, data.password}
		}
	}
	for id, service := range config.Postgres {
		if service.Type == "docker" {
			for _, app := range config.Apps {
				if app.Postgres.Name != id {
					continue
				}
				for _, client := range app.Servers {
					if client == service.Server {
						continue
					}
					bind, _, _ := environment.CommonInternalAddress(config, service.Server, client)
					key := service.Server + ":" + bind + ":" + fmt.Sprint(*service.Port)
					if owner, exists := sockets[key]; exists && owner != "postgres:"+id {
						return fmt.Errorf("Docker data socket collision on %s", key)
					}
					sockets[key] = "postgres:" + id
				}
			}
		}
	}
	for id, service := range config.Redis {
		if service.Type == "docker" {
			for _, app := range config.Apps {
				if app.Redis.Name != id {
					continue
				}
				for _, client := range app.Servers {
					if client == service.Server {
						continue
					}
					bind, _, _ := environment.CommonInternalAddress(config, service.Server, client)
					key := service.Server + ":" + bind + ":" + fmt.Sprint(*service.Port)
					if owner, exists := sockets[key]; exists && owner != "redis:"+id {
						return fmt.Errorf("Docker data socket collision on %s", key)
					}
					sockets[key] = "redis:" + id
				}
			}
		}
	}
	return nil
}

var postgresIdentifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func validPostgresIdentifier(value string) bool {
	return postgresIdentifierPattern.MatchString(value) && !strings.HasPrefix(value, "s2h_")
}
func validPostgresDatabase(value string) bool {
	return postgresIdentifierPattern.MatchString(value) && value != "postgres" && value != "template0" && value != "template1"
}
func validPostgresDSNPassword(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.HasPrefix(value, "'") || strings.ContainsAny(value, "\x00\r\n\\") {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
func portOrZero(port *int) int {
	if port == nil {
		return 0
	}
	return *port
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

func hostTarget(config environment.Config, secrets environment.Secrets, release, server string, managed map[string]managedRedisInputs) pulumi.Map {
	apps := pulumi.Array{}
	for _, id := range sortedAppIDs(config) {
		app := config.Apps[id]
		if !contains(app.Servers, server) {
			continue
		}
		servers := appPlacementOrder(config, app)
		links := pulumi.Array{postgresLink(app.Postgres.Name, app.Postgres.Database, config.Postgres[app.Postgres.Name], server, config)}
		redis := config.Redis[app.Redis.Name]
		if redis.Type == "upstash" {
			links = append(links, managedRedisLink(app.Redis.Name, fmt.Sprint(app.Redis.Database), managed[app.Redis.Name]))
		} else {
			links = append(links, redisLink(app.Redis.Name, fmt.Sprint(app.Redis.Database), redis, server, config))
		}
		settings := copyStringMap(app.Environment)
		apps = append(apps, pulumi.Map{
			"id":                pulumi.String(id),
			"image":             pulumi.String(app.Image),
			"hostname":          pulumi.String(app.Hostname),
			"readinessPath":     pulumi.String(app.ReadinessPath),
			"drainTimeout":      pulumi.String(app.DrainTimeout.String()),
			"initialBootstrap":  pulumi.Bool(server == servers[0]),
			"initialAdminEmail": pulumi.String(app.InitialAdminEmail),
			"runtimeSettings":   stringMapInput(settings),
			"dataLinks":         links,
		})
	}
	services := localServices(config, secrets, server)
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

func postgresLink(id, database string, service environment.Postgres, appServer string, config environment.Config) pulumi.Map {
	endpoint := service.Host
	providerID := id
	if service.Type == "docker" {
		endpoint = id
		providerID = "docker:" + service.Server + ":" + id
		if service.Server != appServer {
			endpoint, _, _ = environment.CommonInternalAddress(config, service.Server, appServer)
		}
	}
	return pulumi.Map{"name": pulumi.String(id), "identity": pulumi.Map{
		"kind":          pulumi.String("postgres"),
		"providerId":    pulumi.String(providerID),
		"endpoint":      pulumi.String(endpoint),
		"port":          pulumi.Int(*service.Port),
		"database":      pulumi.String(database),
		"tlsMode":       pulumi.String(postgresTLS(service)),
		"tlsServerName": pulumi.String(postgresTLSServerName(service)),
	}}
}

func redisLink(id, database string, service environment.Redis, appServer string, config environment.Config) pulumi.Map {
	endpoint := service.Host
	providerID := id
	if service.Type == "docker" {
		endpoint = id
		providerID = "docker:" + service.Server + ":" + id
		if service.Server != appServer {
			endpoint, _, _ = environment.CommonInternalAddress(config, service.Server, appServer)
		}
	}
	return pulumi.Map{"name": pulumi.String(id), "identity": pulumi.Map{
		"kind":       pulumi.String("redis"),
		"providerId": pulumi.String(providerID),
		"endpoint":   pulumi.String(endpoint),
		"port":       pulumi.Int(*service.Port),
		"database":   pulumi.String(database),
		"tlsMode":    pulumi.String(redisTLS(service)),
	}}
}

func managedRedisLink(id, database string, service managedRedisInputs) pulumi.Map {
	return pulumi.Map{"name": pulumi.String(id), "identity": pulumi.Map{
		"kind":       pulumi.String("redis"),
		"providerId": service.ProviderID,
		"endpoint":   service.Endpoint,
		"port":       service.Port,
		"database":   pulumi.String(database),
		"tlsMode":    pulumi.String("require"),
	}}
}
func postgresTLS(service environment.Postgres) string {
	if service.Type == "docker" {
		return "disable"
	}
	return service.TLS.Mode
}
func postgresTLSServerName(service environment.Postgres) string {
	if service.Type != "docker" && (service.TLS.Mode == "verify-ca" || service.TLS.Mode == "verify-full") {
		return service.Host
	}
	return ""
}
func redisTLS(service environment.Redis) string {
	if service.Type == "docker" || service.TLS == nil || !*service.TLS {
		return "disable"
	}
	return "require"
}

func localServices(config environment.Config, secrets environment.Secrets, server string) pulumi.Array {
	services := pulumi.Array{}
	for _, id := range sortedPostgresIDs(config) {
		service := config.Postgres[id]
		if service.Type == "docker" && service.Server == server {
			services = append(services, localService(config, secrets, id, "postgres", *service.Port, false, server))
		}
	}
	for _, id := range sortedRedisIDs(config) {
		service := config.Redis[id]
		if service.Type == "docker" && service.Server == server {
			services = append(services, localService(config, secrets, id, "redis", *service.Port, *service.Persistence, server))
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
			"jwtSecret":          pulumi.String(appSecret.JWTSecret),
			"totpEncryptionKey":  pulumi.String(appSecret.TOTPEncryptionKey),
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
		if server == appPlacementOrder(config, app)[0] {
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
			local[id] = localSecrets(config, secrets, id, "postgres")
		}
	}
	for _, id := range sortedRedisIDs(config) {
		service := config.Redis[id]
		if service.Type == "docker" && service.Server == server {
			local[id] = localSecrets(config, secrets, id, "redis")
		}
	}
	return pulumi.ToSecret(pulumi.Map{
		"reverseProxy":      pulumi.Map{"dnsChallengeToken": pulumi.String(secrets.ReverseProxy.DNSChallengeToken)},
		"apps":              apps,
		"localDataServices": local,
	})
}

func hostDependencies(server string, config environment.Config, hosts map[string]*hostResource, dockerDependencies map[string]map[string]bool) []pulumi.Resource {
	dependencies := []pulumi.Resource{}
	seen := map[string]bool{}
	for _, id := range sortedAppIDs(config) {
		servers := appPlacementOrder(config, config.Apps[id])
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
	for _, owner := range sortedStrings(mapKeys(dockerDependencies[server])) {
		if !seen[owner] {
			dependencies = append(dependencies, hosts[owner])
			seen[owner] = true
		}
	}
	return dependencies
}

func hostOrder(config environment.Config) ([]string, error) {
	return hostOrderWithDockerDataDependencies(config, dockerDataOwnerDependencies(config))
}

func hostOrderWithDockerDataDependencies(config environment.Config, dockerDependencies map[string]map[string]bool) ([]string, error) {
	deps := map[string]map[string]bool{}
	for id := range config.Servers {
		deps[id] = map[string]bool{}
	}
	for server, owners := range dockerDependencies {
		for owner := range owners {
			deps[server][owner] = true
		}
	}
	var order []string
	for len(deps) > 0 {
		var ready []string
		for id, d := range deps {
			if len(d) == 0 {
				ready = append(ready, id)
			}
		}
		if len(ready) == 0 {
			return nil, fmt.Errorf("Host dependency cycle")
		}
		sort.Strings(ready)
		for _, id := range ready {
			order = append(order, id)
			delete(deps, id)
			for _, d := range deps {
				delete(d, id)
			}
		}
	}
	return order, nil
}

// dockerDataOwnerDependencies retains data-owner ordering while an App is absent
// from its final runtime placement, so targeted removal cannot revoke admission first.
func dockerDataOwnerDependencies(config environment.Config) map[string]map[string]bool {
	dependencies := map[string]map[string]bool{}
	for _, app := range config.Apps {
		owners := []string{}
		if service, ok := config.Postgres[app.Postgres.Name]; ok && service.Type == "docker" {
			owners = append(owners, service.Server)
		}
		if service, ok := config.Redis[app.Redis.Name]; ok && service.Type == "docker" {
			owners = append(owners, service.Server)
		}
		consumers := app.Servers
		if len(consumers) == 0 {
			consumers = mapKeys(config.Servers)
		}
		for _, consumer := range consumers {
			for _, owner := range owners {
				if owner == "" || owner == consumer {
					continue
				}
				if dependencies[consumer] == nil {
					dependencies[consumer] = map[string]bool{}
				}
				dependencies[consumer][owner] = true
			}
		}
	}
	return dependencies
}

func appPlacementOrder(config environment.Config, app environment.App) []string {
	order, _ := hostOrder(config)
	rank := map[string]int{}
	for index, server := range order {
		rank[server] = index
	}
	servers := append([]string(nil), app.Servers...)
	sort.Slice(servers, func(i, j int) bool { return rank[servers[i]] < rank[servers[j]] })
	return servers
}

func localService(config environment.Config, secrets environment.Secrets, id, kind string, port int, persistence bool, owner string) pulumi.Map {
	bindingSources := map[string]map[string]bool{}
	clients := pulumi.Array{}
	for _, appID := range sortedAppIDs(config) {
		app := config.Apps[appID]
		name, database, username := app.Postgres.Name, app.Postgres.Database, secrets.Apps[appID].Postgres.Username
		if kind == "redis" {
			name, database, username = app.Redis.Name, fmt.Sprint(app.Redis.Database), secrets.Apps[appID].Redis.Username
		}
		if name != id {
			continue
		}
		clients = append(clients, pulumi.Map{"appId": pulumi.String(appID), "username": pulumi.String(username), "database": pulumi.String(database)})
		for _, client := range app.Servers {
			if client != owner {
				bind, source, _ := environment.CommonInternalAddress(config, owner, client)
				if bindingSources[bind] == nil {
					bindingSources[bind] = map[string]bool{}
				}
				bindingSources[bind][source] = true
			}
		}
	}
	bindings := pulumi.Array{}
	for _, bind := range sortedStrings(mapKeys(bindingSources)) {
		sources := pulumi.Array{}
		for _, source := range sortedStrings(mapKeys(bindingSources[bind])) {
			sources = append(sources, pulumi.String(source))
		}
		bindings = append(bindings, pulumi.Map{"address": pulumi.String(bind), "allowedSources": sources})
	}
	return pulumi.Map{"id": pulumi.String(id), "type": pulumi.String(kind), "port": pulumi.Int(port), "persistence": pulumi.Bool(persistence), "bindings": bindings, "clients": clients}
}
func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
func localSecrets(config environment.Config, secrets environment.Secrets, id, kind string) pulumi.Map {
	passwords := pulumi.Map{}
	admin := ""
	if kind == "postgres" {
		admin = secrets.Postgres[id].AdminPassword
	} else {
		admin = secrets.Redis[id].AdminPassword
	}
	for appID, app := range config.Apps {
		name := app.Postgres.Name
		password := ""
		if secrets.Apps[appID].Postgres != nil {
			password = secrets.Apps[appID].Postgres.Password
		}
		if kind == "redis" {
			name = app.Redis.Name
			if secrets.Apps[appID].Redis != nil {
				password = secrets.Apps[appID].Redis.Password
			}
		}
		if name == id {
			passwords[appID] = pulumi.String(password)
		}
	}
	return pulumi.Map{"adminPassword": pulumi.String(admin), "clientPasswords": passwords}
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
