# Artifactory repository size exporter

A small Prometheus exporter that reads repository storage totals from
Artifactory's Storage Info rest api (`GET /artifactory/api/storageinfo`) and
root-folder properties through AQL (`POST /artifactory/api/search/aql`). It
groups repository storage by the configured property value.

## Configure and run

Set `ARTIFACTORY_PROPERTY_NAME` to the property to read from each repository's
root folder. Configure one or more Artifactory targets with
`ARTIFACTORY_TARGETS`, a JSON array. Each target has a required `url` and may
set a `server` label plus its own `token` or `username` and `password`.
For Kubernetes, use `tokenEnv` instead of `token` to name an environment
variable populated from a Secret; the token value then stays out of the target
JSON stored in a ConfigMap.
Every metric includes a `server` label; it defaults to that target's URL.
Server labels must be unique. For a single target, `ARTIFACTORY_URL`,
`ARTIFACTORY_SERVER`, `ARTIFACTORY_TOKEN`, `ARTIFACTORY_USERNAME`, and
`ARTIFACTORY_PASSWORD` variables remain supported.

`ARTIFACTORY_URL` values should not include a trailing `/artifactory` path.
Set `ARTIFACTORY_PROPERTY_GROUP_SIZE_METRIC_NAME` to change the grouped-size
metric name. If unset or blank, it defaults to
`artifactory_property_group_size_bytes`. Set
`ARTIFACTORY_PROPERTY_VALUE_LABEL_NAME` to change the grouped metric's property
label name. If unset or blank, it defaults to `property_value`.

For authentication, set `ARTIFACTORY_TOKEN` to an access token, or set both
`ARTIFACTORY_USERNAME` and `ARTIFACTORY_PASSWORD`. The token takes precedence
if both authentication methods are configured. Leave them unset only if the
Artifactory server permits anonymous access.

```sh
export ARTIFACTORY_PROPERTY_NAME=team
export ARTIFACTORY_PROPERTY_GROUP_SIZE_METRIC_NAME=artifactory_property_group_size_bytes
export ARTIFACTORY_PROPERTY_VALUE_LABEL_NAME=property_value
export ARTIFACTORY_TARGETS='[{"url":"https://artifactory-one.example.com","server":"production","tokenEnv":"ARTIFACTORY_TOKEN_PRODUCTION"},{"url":"https://artifactory-two.example.com","server":"staging","tokenEnv":"ARTIFACTORY_TOKEN_STAGING"}]'
go run .
```

In Kubernetes, store `ARTIFACTORY_TARGETS` and other non-secret settings in a
ConfigMap, then expose `ARTIFACTORY_TOKEN_PRODUCTION` and
`ARTIFACTORY_TOKEN_STAGING` from a Secret as container environment variables.

The service listens on `:9657` by default; set `LISTEN_ADDR` to change it.
Prometheus metrics are served at `/metrics`, and `/healthz` is a process health
check. Repository sizes and root-folder properties are refreshed on each
Prometheus scrape.

## Metrics

- `artifactory_property_group_size_bytes{property_value}`: total storage for
  repositories with that root-folder property value. If a repository has
  multiple comma- or semicolon-separated values, its full size contributes to
  each resulting group. Non-alphanumeric characters at the beginning and end
  of each value are trimmed before grouping. Empty values are included in the
  missing-property total.
- `artifactory_repository_size_missing_property_bytes`: total storage for
  all repositories whose root folder has no value for the configured property.
  Their sizes are excluded from property-value groups and added together in
  this single metric; the missing property is not logged as an error.
- `artifactory_repository_special_size_bytes{repoKey}`: storage for each
  Artifactory storage-summary entry whose `repoType` is `NA`. This includes
  special entries such as `TOTAL`, `auto-trashcan`, and `jfrog-support-bundle`;
  `TOTAL` is an aggregate and should not be added to the other entries.
- `artifactory_up`: 1 when the most recent Artifactory request succeeded,
  otherwise 0.
- `artifactory_scrape_timestamp_seconds`: Unix time of the most recent API
  request.

For example, a successful scrape from the `production` server could return:

```text
artifactory_property_group_size_bytes{server="production",property_value="blue"} 1048576
artifactory_property_group_size_bytes{server="production",property_value="green"} 2097152
artifactory_repository_size_missing_property_bytes{server="production"} 512000
artifactory_repository_special_size_bytes{server="production",repoKey="TOTAL"} 3849728
artifactory_repository_special_size_bytes{server="production",repoKey="auto-trashcan"} 128000
artifactory_repository_special_size_bytes{server="production",repoKey="jfrog-support-bundle"} 64000
artifactory_up{server="production"} 1
artifactory_scrape_timestamp_seconds{server="production"} 1760000000
```

Repository sizes use Artifactory's numeric `usedSpaceInBytes` field directly,
with no unit conversion. Repositories with an invalid or negative byte count
are skipped and logged.
