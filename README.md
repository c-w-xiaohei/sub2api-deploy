# Sub2API Environment Controller

## Controller Interface

Use the controller-side `sub2api-deploy` CLI to stage Environment configuration and secrets,
attach the Provider, and manage the SSH Host Runtime. The Host server model names SSH aliases and address roles; applications select their
attached Hosts through the Environment configuration.

Start from `Pulumi.production.example.yaml`, then provide the encrypted
`environmentSecrets` value through the controller's protected input path. Review
every `preview` before `up`; `delete` requires the controller's explicit safety
approval and should follow a backup and recovery review.

The managed import command is:

```bash
sub2api-deploy pulumi production import sub2api-host:index:Host host-edge edge
```

The release inventory is `bin/sub2api-deploy`, `bin/pulumi-program`,
`bin/pulumi-resource-sub2api-host`, the Pulumi wrappers, Host Runtime artifacts
for Linux amd64 and arm64, and the public Pulumi and Go metadata files; managed Upstash is supported.

## Capability Boundary

Neon is blocked. Multi-Host app placement is supported. Cross-Host local Docker data and allowlist connectivity are blocked. MicroSocks is blocked.
Tunnel support is blocked. Migration is not performed by this release.
