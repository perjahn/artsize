# Artifactory repository size exporter

A small Prometheus exporter that reads repository storage totals from
Artifactory's Storage Info REST API (`GET /artifactory/api/storageinfo`) and
groups repository storage by a value read from a yaml metadata file for each
repository.

## Configure and run

Configure all targets in the required `ARTIFACTORY_TARGETS` json array. Each
target has a required `url`, `metadataUrl`, and `metadataGroupKey`.
The metadata URL must contain `{repo}`, which is replaced with that target's
repository key. The grouping key names a top-level yaml field whose scalar
value will group repository size. A missing yaml document (HTTP 404), missing
key, or empty value is counted as ungrouped. Other request failures and invalid
yaml fail that target's scrape. Each target may also configure a `server` label
and its own Artifactory `token` or `username` and `password`.

For Kubernetes, use `tokenEnv` instead of `token`, `passwordEnv` instead of
`password`, `metadataTokenEnv` instead of `metadataToken`, and
`metadataPasswordEnv` instead of `metadataPassword` to refer to
environment variables populated from a Secret. This keeps token and password values out of
the target json stored in a ConfigMap. Every metric includes a `server` label;
it defaults to that target's url. Server labels must be unique.

Target `url` values should not include a trailing `/artifactory` path.
Set `ARTIFACTORY_PROPERTY_GROUP_SIZE_METRIC_NAME` to change the grouped-size
metric name. If unset or blank, it defaults to
`artifactory_property_group_size_bytes`. Set
`ARTIFACTORY_PROPERTY_VALUE_LABEL_NAME` to change the grouped metric's property
label name. If unset or blank, it defaults to `property_value`.

Set a target's optional `metadataCacheVersionUrl` to enable caching and
`metadataCacheVersionField` to name the root-level field in its
json response. Both settings must be provided together. The endpoint is checked
on every scrape. If the field value is unchanged, cached grouping values are
reused indefinitely; when it changes, metadata is fetched again for every
repository and the cache is replaced once all requests succeed. Missing
documents and empty values are cached too; failed requests and invalid yaml
are not. Storage totals are always refreshed on each scrape.

Metadata requests and the optional cache-version request share per-target
credentials, separate from Artifactory credentials. Configure a metadata token
with `metadataToken` or `metadataTokenEnv`, or configure basic auth with
`metadataUsername` and either `metadataPassword` or `metadataPasswordEnv`.
Likewise, Artifactory basic auth uses `username` with either `password` or
`passwordEnv`; alternatively, configure `token` or `tokenEnv`.

Each `*PasswordEnv` and `*TokenEnv` field names an environment variable that
must be set and non-empty. Its corresponding literal field must be omitted.
Basic authentication requires the username and password together. Leave
credentials unset for anonymous access. When both token and basic auth are
configured, the token takes precedence.

```sh
export ARTIFACTORY_PROPERTY_GROUP_SIZE_METRIC_NAME=artifactory_property_group_size_bytes
export ARTIFACTORY_PROPERTY_VALUE_LABEL_NAME=property_value
export ARTIFACTORY_TARGETS='[{"url":"https://artifactory-one.example.com","server":"production","tokenEnv":"ARTIFACTORY_TOKEN_PRODUCTION","metadataUrl":"https://metadata-one.example.com/repos/{repo}.yaml","metadataGroupKey":"team","metadataCacheVersionUrl":"https://metadata-one.example.com/version.json","metadataCacheVersionField":"revision","metadataTokenEnv":"METADATA_TOKEN_PRODUCTION"},{"url":"https://artifactory-two.example.com","server":"staging","tokenEnv":"ARTIFACTORY_TOKEN_STAGING","metadataUrl":"https://metadata-two.example.com/repos/{repo}.yaml","metadataGroupKey":"owner","metadataCacheVersionUrl":"https://metadata-two.example.com/version.json","metadataCacheVersionField":"revision","metadataTokenEnv":"METADATA_TOKEN_STAGING"}]'
go run .
```

In Kubernetes, store `ARTIFACTORY_TARGETS` and other non-secret settings in a
ConfigMap, then expose `ARTIFACTORY_TOKEN_PRODUCTION` and
`ARTIFACTORY_TOKEN_STAGING`, `METADATA_TOKEN_PRODUCTION`, and
`METADATA_TOKEN_STAGING` from a Secret as container environment variables.

The service listens on `:9657` by default; set `LISTEN_ADDR` to change it.
Prometheus metrics are served at `/metrics`, and `/healthz` is a process health
check. Repository sizes and the metadata cache version are refreshed on each
Prometheus scrape. The Artifactory authentication configured for each target
is also used for its metadata and cache-version requests.
Each incoming http request is logged with its duration and cumulative
repository/cache counters, including repositories seen, eligible and
ungrouped, cache hits/misses/refreshes, metadata fetches and errors.

## Metrics

- `artifactory_property_group_size_bytes{property_value}`: total storage for
  repositories whose yaml document has the configured grouping key and value.
  The metric and label names remain configurable for compatibility. A
  repository contributes its size to one group.
- `artifactory_repository_size_missing_property_bytes`: total storage for
  repositories whose yaml document is missing, has no configured key, or has
  an empty value. Their sizes are excluded from groups and added together in
  this single metric; missing metadata is not logged as an error.
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
