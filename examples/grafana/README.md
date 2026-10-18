# Grafana dashboard

`flats-overview.json` is an example Grafana dashboard for the host metrics
Flats serves at `/metrics` ([Metrics](../../docs/design.md#metrics)). It was
checked with Grafana 11.6 and Prometheus 3.5.

## Import

1. Scrape the host with Prometheus as described in
   [Scraping metrics](../../docs/networking.md#scraping-metrics).
2. In Grafana, open **Dashboards → New → Import** and upload
   `flats-overview.json`. The **Data source** variable at the top of the
   dashboard uses your default Prometheus data source; pick another there if
   needed.

To provision it instead, put the file in a directory listed by a dashboard
provider (`/etc/grafana/provisioning/dashboards/*.yml`). No data source UID
needs changing, since panels use the **Data source** variable.

## Panels

- **Overview:** flats, running flats, pending approvals, network endpoints
  that are not ready, worker crashes, the age of the host's last sweep, and
  host-wide request rate, `5xx` ratio, p95 response time, worker CPU and
  memory, and `flats.db` size.
- **Traffic:** requests by flat and by status class, `5xx` ratio, p50 and p95
  response time, rate-limited requests, open WebSocket and event streams, and
  response bandwidth.
- **Workers:** CPU by flat and role (live, preview, check), memory, current
  worker state, unexpected exits and memory sample age.
- **Storage & network:** disk per flat, route endpoints by provider and
  state, and `flats.db` growth.
- **Management server & host process:** API, console and MCP requests and
  errors, and the Flats process's own CPU and memory.

The **Flat** variable narrows the per-flat panels. Worker memory is measured
on Linux only, and a `closed` request (the visitor left before the answer) is
not an error. A request that ends at the 10-second runtime deadline falls in
the 10–11 s histogram bucket, so p95 reads at most about 11 s for flats with
many timeouts.
