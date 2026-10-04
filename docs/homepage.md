# Homepage integration

DebridUp exposes a read-only integration for [Homepage](https://gethomepage.dev/).
It uses the dashboard's existing metrics and history; requests never run checks or modify monitoring data.

## Enable

Generate a separate random credential (for example, `openssl rand -hex 32`). Set these environment variables on the DebridUp container, then recreate it:

```yaml
environment:
  DEBRIDUP_HOMEPAGE_TOKEN: ${DEBRIDUP_HOMEPAGE_TOKEN}
  DEBRIDUP_HOMEPAGE_ORIGIN: https://homepage.example.com
```

The token must contain at least 32 characters. Without it the integration returns 404.
The origin is optional for JSON widgets, required for charts, and must be the exact browser origin of Homepage (scheme, hostname, optional port, no trailing slash).
Keep the existing admin password and encryption settings. This credential cannot authorize admin API operations.

## Counters

Add to Homepage's `services.yaml`, substituting your DebridUp URL. Store the same credential as `HOMEPAGE_VAR_DEBRIDUP_TOKEN` in Homepage's environment:

```yaml
- Monitoring:
    - DebridUp:
        href: https://debridup.example.com
        widget:
          type: customapi
          url: https://debridup.example.com/integrations/homepage?range=24h
          refreshInterval: 60000
          headers:
            Authorization: Bearer {{HOMEPAGE_VAR_DEBRIDUP_TOKEN}}
          mappings:
            - field: summary.providersOnline
              label: Online
              format: number
            - field: summary.activeIncidents
              label: Incidents
              format: number
            - field: summary.checksToday
              label: Checks today
              format: number
```

## Provider list

Use the same `type`, `url`, `headers`, and `refreshInterval` above with:

```yaml
display: dynamic-list
mappings:
  items: providers
  name: name
  label: state
  format: text
  limit: 11
```

Each item represents a configured monitor, so multiple accounts for one provider appear separately.

## Graphs

Homepage's Custom API widget displays values and lists, not arbitrary time-series charts. Use its [iframe widget](https://gethomepage.dev/widgets/services/iframe/) for the chart endpoint:

```yaml
- Monitoring:
    - DebridUp latency:
        widget:
          type: iframe
          name: DebridUp latency
          src: https://debridup.example.com/integrations/homepage/chart?range=24h&metric=p95&token={{HOMEPAGE_VAR_DEBRIDUP_TOKEN}}
          classes: h-80
          referrerPolicy: no-referrer
          allowScrolling: yes
          refreshInterval: 60000
```

Use `metric=availability` for an availability graph. Both endpoints accept `range=24h` (default), `7d`, or `30d`. Charts keep a consistent color per provider and show gaps where observations are missing. They refresh through Homepage's iframe refresh interval.

The iframe URL is loaded by the viewer's browser, so it needs a browser-reachable address, HTTPS when Homepage uses HTTPS, and must not sit behind a login redirect. Unlike server-side Custom API requests, iframe requests cannot supply an Authorization header: the read-only token is in the URL and visible to Homepage viewers and potentially reverse-proxy access logs. Use this only with trusted viewers, avoid logging query strings, and rotate the token by updating both services. No provider credentials, raw errors, or incident descriptions are returned; monitor names and metrics are visible to anyone with the integration token. The admin dashboard remains non-embeddable.

## JSON contract

`GET /integrations/homepage` requires `Authorization: Bearer <credential>`; query-string authentication is accepted only on `/integrations/homepage/chart`.

The response contains `generatedAt` (Unix seconds), `range`, `summary` (`overallState`, `providersOnline`, `activeIncidents`, `checksToday`), and `providers`. Each provider contains `id`, `name`, `provider`, `state`, `stateSince`, `lastCheck`, `availability` (0–100), `p50Ms`, `p95Ms`, `slowestMs`, and `series`. Each series bucket has `bucketStart`, `state`, `totalChecks`, `healthyChecks`, `availability`, `p50Ms`, and `p95Ms`. Missing measurements remain JSON null, never synthetic zeroes. States, bucket alignment, percentile aggregation, and today's check count follow the dashboard exactly.

Responses are uncached (`Cache-Control: no-store`). Invalid credentials return 401, invalid range/metric returns 400, and unavailable history returns 503. The integration supports GET/HEAD only. See [Homepage Custom API documentation](https://gethomepage.dev/widgets/services/customapi/) for more mappings.
