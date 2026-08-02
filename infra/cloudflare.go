package main

import (
	"github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type DomainResources struct {
	ZoneID      pulumi.StringOutput
	DNSRecordID pulumi.IDOutput
	DNSRecord   *cloudflare.DnsRecord
	Provider    *cloudflare.Provider
}

func CreateDomainResources(ctx *pulumi.Context, domain, originIP, zoneID string, apiToken pulumi.StringInput) (DomainResources, error) {
	provider, err := cloudflare.NewProvider(ctx, "cloudflare", &cloudflare.ProviderArgs{
		ApiToken: apiToken.ToStringPtrOutput(),
	}, pulumi.Version("6.18.0"))
	if err != nil {
		return DomainResources{}, err
	}
	record, err := cloudflare.NewDnsRecord(ctx, "sub2api-origin", &cloudflare.DnsRecordArgs{
		ZoneId:  pulumi.String(zoneID),
		Name:    pulumi.String(domain),
		Type:    pulumi.String(recordType(originIP)),
		Content: pulumi.String(originIP),
		Proxied: pulumi.Bool(true),
		Ttl:     pulumi.Float64(1),
	}, pulumi.Provider(provider), pulumi.Version("6.18.0"))
	if err != nil {
		return DomainResources{}, err
	}
	return DomainResources{ZoneID: pulumi.String(zoneID).ToStringOutput(), DNSRecordID: record.ID(), DNSRecord: record, Provider: provider}, nil
}

func recordType(originIP string) string {
	for _, character := range originIP {
		if character == ':' {
			return "AAAA"
		}
	}
	return "A"
}

func CreateStrictSSLSetting(ctx *pulumi.Context, domain DomainResources, originReady pulumi.Resource) (*cloudflare.ZoneSetting, error) {
	dependencies := []pulumi.Resource{domain.DNSRecord}
	if originReady != nil {
		dependencies = append(dependencies, originReady)
	}
	return cloudflare.NewZoneSetting(ctx, "cloudflare-full-strict", &cloudflare.ZoneSettingArgs{
		ZoneId:    domain.ZoneID,
		SettingId: pulumi.String("ssl"),
		Value:     pulumi.String("strict"),
	}, pulumi.Provider(domain.Provider), pulumi.Version("6.18.0"), pulumi.DependsOn(dependencies))
}
