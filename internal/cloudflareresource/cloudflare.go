// Package cloudflareresource registers the Cloudflare resources this program uses.
package cloudflareresource

import (
	"errors"
	"reflect"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const version = "6.18.0"

type Provider struct{ pulumi.ProviderResourceState }

type ProviderArgs struct {
	ApiKey            pulumi.StringPtrInput `pulumi:"apiKey"`
	ApiToken          pulumi.StringPtrInput `pulumi:"apiToken"`
	ApiUserServiceKey pulumi.StringPtrInput `pulumi:"apiUserServiceKey"`
}

type providerArgs struct {
	ApiKey            *string `pulumi:"apiKey"`
	ApiToken          *string `pulumi:"apiToken"`
	ApiUserServiceKey *string `pulumi:"apiUserServiceKey"`
}

func (ProviderArgs) ElementType() reflect.Type { return reflect.TypeOf((*providerArgs)(nil)).Elem() }

func NewProvider(ctx *pulumi.Context, name string, args *ProviderArgs, opts ...pulumi.ResourceOption) (*Provider, error) {
	if args == nil {
		args = &ProviderArgs{}
	}
	if args.ApiKey != nil {
		args.ApiKey = pulumi.ToSecret(args.ApiKey).(pulumi.StringPtrInput)
	}
	if args.ApiToken != nil {
		args.ApiToken = pulumi.ToSecret(args.ApiToken).(pulumi.StringPtrInput)
	}
	if args.ApiUserServiceKey != nil {
		args.ApiUserServiceKey = pulumi.ToSecret(args.ApiUserServiceKey).(pulumi.StringPtrInput)
	}
	opts = append(opts, pulumi.AdditionalSecretOutputs([]string{"apiKey", "apiToken", "apiUserServiceKey"}))
	opts = append([]pulumi.ResourceOption{pulumi.Version(version)}, opts...)
	resource := &Provider{}
	if err := ctx.RegisterResource("pulumi:providers:cloudflare", name, args, resource, opts...); err != nil {
		return nil, err
	}
	return resource, nil
}

type DnsRecord struct{ pulumi.CustomResourceState }

type DnsRecordArgs struct {
	Content pulumi.StringPtrInput `pulumi:"content"`
	Name    pulumi.StringInput    `pulumi:"name"`
	Proxied pulumi.BoolPtrInput   `pulumi:"proxied"`
	Ttl     pulumi.Float64Input   `pulumi:"ttl"`
	Type    pulumi.StringInput    `pulumi:"type"`
	ZoneId  pulumi.StringInput    `pulumi:"zoneId"`
}

type dnsRecordArgs struct {
	Content *string `pulumi:"content"`
	Name    string  `pulumi:"name"`
	Proxied *bool   `pulumi:"proxied"`
	Ttl     float64 `pulumi:"ttl"`
	Type    string  `pulumi:"type"`
	ZoneId  string  `pulumi:"zoneId"`
}

func (DnsRecordArgs) ElementType() reflect.Type { return reflect.TypeOf((*dnsRecordArgs)(nil)).Elem() }

func NewDnsRecord(ctx *pulumi.Context, name string, args *DnsRecordArgs, opts ...pulumi.ResourceOption) (*DnsRecord, error) {
	if args == nil {
		return nil, errors.New("missing one or more required arguments")
	}
	if args.Name == nil {
		return nil, errors.New("invalid value for required argument 'Name'")
	}
	if args.Ttl == nil {
		return nil, errors.New("invalid value for required argument 'Ttl'")
	}
	if args.Type == nil {
		return nil, errors.New("invalid value for required argument 'Type'")
	}
	if args.ZoneId == nil {
		return nil, errors.New("invalid value for required argument 'ZoneId'")
	}
	opts = append(opts, pulumi.Aliases([]pulumi.Alias{{Type: pulumi.String("cloudflare:index/record:Record")}}))
	opts = append([]pulumi.ResourceOption{pulumi.Version(version)}, opts...)
	resource := &DnsRecord{}
	if err := ctx.RegisterResource("cloudflare:index/dnsRecord:DnsRecord", name, args, resource, opts...); err != nil {
		return nil, err
	}
	return resource, nil
}

type ZoneSetting struct{ pulumi.CustomResourceState }

type ZoneSettingArgs struct {
	SettingId pulumi.StringInput `pulumi:"settingId"`
	Value     pulumi.Input       `pulumi:"value"`
	ZoneId    pulumi.StringInput `pulumi:"zoneId"`
}

type zoneSettingArgs struct {
	SettingId string      `pulumi:"settingId"`
	Value     interface{} `pulumi:"value"`
	ZoneId    string      `pulumi:"zoneId"`
}

func (ZoneSettingArgs) ElementType() reflect.Type {
	return reflect.TypeOf((*zoneSettingArgs)(nil)).Elem()
}

func NewZoneSetting(ctx *pulumi.Context, name string, args *ZoneSettingArgs, opts ...pulumi.ResourceOption) (*ZoneSetting, error) {
	if args == nil {
		return nil, errors.New("missing one or more required arguments")
	}
	if args.SettingId == nil {
		return nil, errors.New("invalid value for required argument 'SettingId'")
	}
	if args.Value == nil {
		return nil, errors.New("invalid value for required argument 'Value'")
	}
	if args.ZoneId == nil {
		return nil, errors.New("invalid value for required argument 'ZoneId'")
	}
	resource := &ZoneSetting{}
	opts = append([]pulumi.ResourceOption{pulumi.Version(version)}, opts...)
	if err := ctx.RegisterResource("cloudflare:index/zoneSetting:ZoneSetting", name, args, resource, opts...); err != nil {
		return nil, err
	}
	return resource, nil
}
