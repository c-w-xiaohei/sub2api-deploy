import * as cloudflare from "@pulumi/cloudflare";
import * as pulumi from "@pulumi/pulumi";

export interface DomainResources {
  zoneId: pulumi.Output<string>;
  dnsRecordId: pulumi.Output<string>;
  dnsRecord: cloudflare.DnsRecord;
  domain: string;
  originReady?: pulumi.Resource;
  provider?: cloudflare.Provider;
}

export function createDomainResources(args: {
  domain: string;
  originIp: string;
  zoneId: string;
  apiToken?: pulumi.Input<string>;
  originReady?: pulumi.Resource;
}): DomainResources {
  const provider = args.apiToken
    ? new cloudflare.Provider("cloudflare", { apiToken: args.apiToken })
    : undefined;
  const zoneId = pulumi.output(args.zoneId);
  const record = new cloudflare.DnsRecord(
    "sub2api-origin",
    {
      zoneId,
      name: args.domain,
      type: args.originIp.includes(":") ? "AAAA" : "A",
      content: args.originIp,
      proxied: true,
      ttl: 1,
    },
    { provider },
  );
  return { zoneId, dnsRecordId: record.id, dnsRecord: record, domain: args.domain, originReady: args.originReady, provider };
}

export function createStrictSslSetting(
  domain: DomainResources,
): cloudflare.ZoneSetting {
  return new cloudflare.ZoneSetting("cloudflare-full-strict", {
    zoneId: domain.zoneId,
    settingId: "ssl",
    value: "strict",
  }, {
    dependsOn: [domain.dnsRecord, ...(domain.originReady ? [domain.originReady] : [])],
    provider: domain.provider,
  });
}
