export interface InfraTriggerInputs {
  domain: string;
  originIp: string;
  postgresMode: string;
  redisMode: string;
  traefikImage: string;
  acmeEmail: string;
  appProbePath: string;
  drainSeconds: number;
  composeChecksum: string;
  resourceModes: string;
}

export function buildInfraTriggers(input: InfraTriggerInputs): string[] {
  return [
    "infra-reconcile-v1",
    input.domain,
    input.originIp,
    input.postgresMode,
    input.redisMode,
    input.traefikImage,
    input.acmeEmail,
    input.appProbePath,
    String(input.drainSeconds),
    input.composeChecksum,
    input.resourceModes,
  ];
}

export function buildReleaseTriggers(sub2apiImage: string): string[] {
  return [sub2apiImage];
}
