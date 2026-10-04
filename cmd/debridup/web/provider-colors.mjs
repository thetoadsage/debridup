const PROVIDERS = Object.freeze(['torbox', 'premiumize', 'alldebrid', 'realdebrid', 'torrin', 'pikpak', 'offcloud', 'debridlink', 'easydebrid', 'debrider', 'deepbrid']);

export function providerColorClass(provider) {
  const known = PROVIDERS.indexOf(provider?.provider);
  const id = Number(provider?.id);
  const index = known >= 0 ? known : Number.isSafeInteger(id) ? Math.abs(id) % PROVIDERS.length : 0;
  return `provider-color-${index}`;
}
