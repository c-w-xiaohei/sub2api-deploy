package main

import (
	"github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func edgeAliases(legacyCode2 bool, oldName string) []pulumi.Alias { if !legacyCode2 { return nil }; return []pulumi.Alias{{Name: pulumi.String(oldName), NoParent: pulumi.Bool(true)}} }

func createCloudflareProvider(ctx *pulumi.Context, edge, preflight pulumi.Resource, apiToken pulumi.StringInput, legacyCode2 bool) (*cloudflare.Provider, error) {
	return cloudflare.NewProvider(ctx, "cloudflare", &cloudflare.ProviderArgs{ApiToken: apiToken.ToStringPtrOutput()}, pulumi.Parent(edge), pulumi.Aliases(edgeAliases(legacyCode2, "cloudflare")), pulumi.DependsOn([]pulumi.Resource{preflight}), pulumi.Version("6.18.0"))
}

func createStrictSSLSetting(ctx *pulumi.Context, edge, preflight pulumi.Resource, provider *cloudflare.Provider, zoneID string, legacyCode2 bool) (*cloudflare.ZoneSetting, error) {
	return cloudflare.NewZoneSetting(ctx, "cloudflare-full-strict", &cloudflare.ZoneSettingArgs{
		ZoneId: pulumi.String(zoneID), SettingId: pulumi.String("ssl"), Value: pulumi.String("strict"),
	}, pulumi.Parent(edge), pulumi.Aliases(edgeAliases(legacyCode2, "cloudflare-full-strict")), pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{preflight}), pulumi.Version("6.18.0"))
}

func createSiteDNSRecord(ctx *pulumi.Context, site, preflight pulumi.Resource, provider *cloudflare.Provider, layout SiteLayout, spec SiteSpec, edge EdgeSpec) (*cloudflare.DnsRecord, error) {
	siteID := layout.SiteID
	return cloudflare.NewDnsRecord(ctx, "site-"+siteID+"-origin", &cloudflare.DnsRecordArgs{
		ZoneId: pulumi.String(edge.CloudflareZoneID), Name: pulumi.String(spec.Domain), Type: pulumi.String(recordType(edge.OriginIP)),
		Content: pulumi.String(edge.OriginIP), Proxied: pulumi.Bool(true), Ttl: pulumi.Float64(1),
	}, pulumi.Parent(site), pulumi.Aliases(legacyCode2Aliases(layout, "sub2api-origin")), pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{preflight}), pulumi.Version("6.18.0"))
}

func recordType(originIP string) string {
	for _, character := range originIP { if character == ':' { return "AAAA" } }
	return "A"
}
