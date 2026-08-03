package main

import (
	"github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func createCloudflareProvider(ctx *pulumi.Context, edge pulumi.Resource, apiToken pulumi.StringInput) (*cloudflare.Provider, error) {
	return cloudflare.NewProvider(ctx, "cloudflare", &cloudflare.ProviderArgs{ApiToken: apiToken.ToStringPtrOutput()}, pulumi.Parent(edge), pulumi.Version("6.18.0"))
}

func createStrictSSLSetting(ctx *pulumi.Context, edge pulumi.Resource, provider *cloudflare.Provider, zoneID string) (*cloudflare.ZoneSetting, error) {
	return cloudflare.NewZoneSetting(ctx, "cloudflare-full-strict", &cloudflare.ZoneSettingArgs{
		ZoneId: pulumi.String(zoneID), SettingId: pulumi.String("ssl"), Value: pulumi.String("strict"),
	}, pulumi.Parent(edge), pulumi.Provider(provider), pulumi.Version("6.18.0"))
}

func createSiteDNSRecord(ctx *pulumi.Context, site pulumi.Resource, provider *cloudflare.Provider, siteID string, spec SiteSpec, edge EdgeSpec) (*cloudflare.DnsRecord, error) {
	return cloudflare.NewDnsRecord(ctx, "site-"+siteID+"-origin", &cloudflare.DnsRecordArgs{
		ZoneId: pulumi.String(edge.CloudflareZoneID), Name: pulumi.String(spec.Domain), Type: pulumi.String(recordType(edge.OriginIP)),
		Content: pulumi.String(edge.OriginIP), Proxied: pulumi.Bool(true), Ttl: pulumi.Float64(1),
	}, pulumi.Parent(site), pulumi.Provider(provider), pulumi.Version("6.18.0"))
}

func recordType(originIP string) string {
	for _, character := range originIP { if character == ':' { return "AAAA" } }
	return "A"
}
