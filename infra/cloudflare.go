package main

import (
	"github.com/c-w-xiaohei/sub2api-deploy/internal/cloudflareresource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func edgeAliases(legacyCode2 bool, oldName string) []pulumi.Alias {
	if !legacyCode2 {
		return nil
	}
	return []pulumi.Alias{{Name: pulumi.String(oldName), NoParent: pulumi.Bool(true)}}
}

func createCloudflareProvider(ctx *pulumi.Context, edge, preflight pulumi.Resource, apiToken pulumi.StringInput, legacyCode2 bool) (*cloudflareresource.Provider, error) {
	return cloudflareresource.NewProvider(ctx, "cloudflare", &cloudflareresource.ProviderArgs{ApiToken: apiToken.ToStringPtrOutput()}, pulumi.Parent(edge), pulumi.Aliases(edgeAliases(legacyCode2, "cloudflare")), pulumi.DependsOn([]pulumi.Resource{preflight}), pulumi.Version("6.18.0"))
}

func createStrictSSLSetting(ctx *pulumi.Context, edge, preflight pulumi.Resource, provider *cloudflareresource.Provider, zoneID string, legacyCode2 bool) (*cloudflareresource.ZoneSetting, error) {
	return cloudflareresource.NewZoneSetting(ctx, "cloudflare-full-strict", &cloudflareresource.ZoneSettingArgs{
		ZoneId: pulumi.String(zoneID), SettingId: pulumi.String("ssl"), Value: pulumi.String("strict"),
	}, pulumi.Parent(edge), pulumi.Aliases(edgeAliases(legacyCode2, "cloudflare-full-strict")), pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{preflight}), pulumi.Version("6.18.0"))
}

func createSiteDNSRecord(ctx *pulumi.Context, site, preflight pulumi.Resource, provider *cloudflareresource.Provider, layout SiteLayout, spec SiteSpec, edge EdgeSpec) (*cloudflareresource.DnsRecord, error) {
	siteID := layout.SiteID
	return cloudflareresource.NewDnsRecord(ctx, "site-"+siteID+"-origin", &cloudflareresource.DnsRecordArgs{
		ZoneId: pulumi.String(edge.CloudflareZoneID), Name: pulumi.String(spec.Domain), Type: pulumi.String(recordType(edge.OriginIP)),
		Content: pulumi.StringPtr(edge.OriginIP), Proxied: pulumi.BoolPtr(true), Ttl: pulumi.Float64(1),
	}, pulumi.Parent(site), pulumi.Aliases(legacyCode2Aliases(layout, "sub2api-origin")), pulumi.Provider(provider), pulumi.DependsOn([]pulumi.Resource{preflight}), pulumi.Version("6.18.0"))
}

func recordType(originIP string) string {
	for _, character := range originIP {
		if character == ':' {
			return "AAAA"
		}
	}
	return "A"
}
