package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func setMetadataConfig(t *testing.T) {
	t.Helper()
	setTargetConfigs(t, defaultTargetConfig())
}

func defaultTargetConfig() artifactoryTargetConfig {
	return artifactoryTargetConfig{
		Url:              "https://artifactory.example.com",
		MetadataUrl:      "https://metadata.example.com/repos/{repo}.yaml",
		MetadataGroupKey: "team",
	}
}

func setTargetConfigs(t *testing.T, targets ...artifactoryTargetConfig) {
	t.Helper()
	configuredTargets, err := json.Marshal(targets)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTIFACTORY_TARGETS", string(configuredTargets))
}

func TestRepositoryMetadataConfiguration(t *testing.T) {
	setMetadataConfig(t)

	clients, err := newArtifactoryClients()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := clients[0].metadataUrlTemplate, "https://metadata.example.com/repos/{repo}.yaml"; got != want {
		t.Errorf("metadata url template = %q, want %q", got, want)
	}
	if got, want := clients[0].metadataGroupKey, "team"; got != want {
		t.Errorf("metadata group key = %q, want %q", got, want)
	}

	t.Run("requires repository placeholder", func(t *testing.T) {
		target := defaultTargetConfig()
		target.MetadataUrl = "https://metadata.example.com/repos/repo.yaml"
		setTargetConfigs(t, target)
		if _, err := newArtifactoryClients(); err == nil {
			t.Fatal("newArtifactoryClients() succeeded without {repo} in metadata url")
		}
	})
	t.Run("requires grouping key", func(t *testing.T) {
		target := defaultTargetConfig()
		target.MetadataGroupKey = " "
		setTargetConfigs(t, target)
		if _, err := newArtifactoryClients(); err == nil {
			t.Fatal("newArtifactoryClients() succeeded without metadata grouping key")
		}
	})
}

func TestRepositoryMetadataCacheConfiguration(t *testing.T) {
	setMetadataConfig(t)

	t.Run("disabled when unset", func(t *testing.T) {
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if clients[0].metadataCache == nil || clients[0].metadataCache.enabled {
			t.Error("metadata cache is enabled without version endpoint configuration")
		}
	})

	t.Run("enabled when both settings are configured", func(t *testing.T) {
		target := defaultTargetConfig()
		target.MetadataCacheVersionUrl = "https://metadata.example.com/version.json"
		target.MetadataCacheVersionField = "revision"
		setTargetConfigs(t, target)
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if clients[0].metadataCache == nil {
			t.Fatal("metadata cache is disabled despite version endpoint configuration")
		}
		if got, want := clients[0].metadataCacheVersionUrl, "https://metadata.example.com/version.json"; got != want {
			t.Errorf("metadata cache version url = %q, want %q", got, want)
		}
		if got, want := clients[0].metadataCacheField, "revision"; got != want {
			t.Errorf("metadata cache field = %q, want %q", got, want)
		}
	})

	t.Run("resolves repository metadata token from environment", func(t *testing.T) {
		target := defaultTargetConfig()
		target.MetadataTokenEnv = "METADATA_VERSION_SECRET"
		setTargetConfigs(t, target)
		t.Setenv("METADATA_VERSION_SECRET", "version-token")
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := clients[0].metadataToken, "version-token"; got != want {
			t.Errorf("metadata token = %q, want configured environment value", got)
		}
	})
	t.Run("ignores global repository metadata variables", func(t *testing.T) {
		t.Setenv("ARTIFACTORY_REPOSITORY_METADATA_TOKEN", "global-token")
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if clients[0].metadataToken != "" {
			t.Errorf("metadata token = %q, want empty target token", clients[0].metadataToken)
		}
	})

	t.Run("requires paired metadata basic credentials", func(t *testing.T) {
		target := defaultTargetConfig()
		target.MetadataUsername = "metadata-user"
		setTargetConfigs(t, target)
		if _, err := newArtifactoryClients(); err == nil {
			t.Fatal("newArtifactoryClients() succeeded with only metadata username")
		}
	})
	t.Run("rejects version token and tokenEnv together", func(t *testing.T) {
		target := defaultTargetConfig()
		target.MetadataToken = "metadata-token"
		target.MetadataTokenEnv = "METADATA_VERSION_SECRET"
		setTargetConfigs(t, target)
		t.Setenv("METADATA_VERSION_SECRET", "version-token-from-env")
		if _, err := newArtifactoryClients(); err == nil {
			t.Fatal("newArtifactoryClients() succeeded with both metadata token settings")
		}
	})

	for _, settings := range []struct {
		name  string
		url   string
		field string
	}{
		{name: "url without field", url: "https://metadata.example.com/version.json"},
		{name: "field without url", field: "revision"},
	} {
		t.Run("rejects "+settings.name, func(t *testing.T) {
			target := defaultTargetConfig()
			target.MetadataCacheVersionUrl = settings.url
			target.MetadataCacheVersionField = settings.field
			setTargetConfigs(t, target)
			if _, err := newArtifactoryClients(); err == nil {
				t.Fatal("newArtifactoryClients() succeeded with incomplete cache version configuration")
			}
		})
	}
}

func TestTargetPasswordEnvironmentVariables(t *testing.T) {
	setMetadataConfig(t)
	target := defaultTargetConfig()
	target.Username = "artifactory-user"
	target.PasswordEnv = "ARTIFACTORY_PASSWORD_SECRET"
	target.MetadataUsername = "metadata-user"
	target.MetadataPasswordEnv = "METADATA_PASSWORD_SECRET"
	setTargetConfigs(t, target)
	t.Setenv("ARTIFACTORY_PASSWORD_SECRET", "artifactory-password")
	t.Setenv("METADATA_PASSWORD_SECRET", "metadata-password")

	clients, err := newArtifactoryClients()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := clients[0].password, "artifactory-password"; got != want {
		t.Errorf("Artifactory password = %q, want resolved secret value", got)
	}
	if got, want := clients[0].metadataPassword, "metadata-password"; got != want {
		t.Errorf("metadata password = %q, want resolved secret value", got)
	}
}

func TestTargetPasswordEnvironmentVariableValidation(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*artifactoryTargetConfig)
		env   string
		value string
	}{
		{
			name: "missing Artifactory password secret",
			setup: func(target *artifactoryTargetConfig) {
				target.Username = "user"
				target.PasswordEnv = "MISSING_ARTIFACTORY_PASSWORD"
			},
		},
		{
			name: "empty metadata password secret",
			setup: func(target *artifactoryTargetConfig) {
				target.MetadataUsername = "user"
				target.MetadataPasswordEnv = "EMPTY_METADATA_PASSWORD"
			},
			env: "EMPTY_METADATA_PASSWORD",
		},
		{
			name: "literal and env Artifactory passwords",
			setup: func(target *artifactoryTargetConfig) {
				target.Username = "user"
				target.Password = "literal"
				target.PasswordEnv = "ARTIFACTORY_PASSWORD_SECRET"
			},
			env:   "ARTIFACTORY_PASSWORD_SECRET",
			value: "secret",
		},
		{
			name: "literal and env metadata passwords",
			setup: func(target *artifactoryTargetConfig) {
				target.MetadataUsername = "user"
				target.MetadataPassword = "literal"
				target.MetadataPasswordEnv = "METADATA_PASSWORD_SECRET"
			},
			env:   "METADATA_PASSWORD_SECRET",
			value: "secret",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setMetadataConfig(t)
			target := defaultTargetConfig()
			test.setup(&target)
			setTargetConfigs(t, target)
			if test.env != "" {
				t.Setenv(test.env, test.value)
			}
			if _, err := newArtifactoryClients(); err == nil {
				t.Fatal("newArtifactoryClients() succeeded with invalid password configuration")
			}
		})
	}
}

func TestPropertyGroupMetricNameConfiguration(t *testing.T) {
	setMetadataConfig(t)
	t.Setenv("ARTIFACTORY_PROPERTY_VALUE_LABEL_NAME", "")

	t.Run("defaults when unset", func(t *testing.T) {
		t.Setenv("ARTIFACTORY_PROPERTY_GROUP_SIZE_METRIC_NAME", "")
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := clients[0].propertyGroupMetricName, "artifactory_property_group_size_bytes"; got != want {
			t.Errorf("property group metric name = %q, want %q", got, want)
		}
	})

	t.Run("uses configured name", func(t *testing.T) {
		t.Setenv("ARTIFACTORY_PROPERTY_GROUP_SIZE_METRIC_NAME", "custom_repo_size_bytes")
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := clients[0].propertyGroupMetricName, "custom_repo_size_bytes"; got != want {
			t.Errorf("property group metric name = %q, want %q", got, want)
		}
	})

	t.Run("rejects invalid name", func(t *testing.T) {
		t.Setenv("ARTIFACTORY_PROPERTY_GROUP_SIZE_METRIC_NAME", "bad metric name")
		if _, err := newArtifactoryClients(); err == nil {
			t.Fatal("newArtifactoryClients() succeeded with invalid metric name")
		}
	})
}

func TestPropertyValueLabelNameConfiguration(t *testing.T) {
	setMetadataConfig(t)

	t.Run("defaults when unset", func(t *testing.T) {
		t.Setenv("ARTIFACTORY_PROPERTY_VALUE_LABEL_NAME", "")
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := clients[0].propertyValueLabelName, "property_value"; got != want {
			t.Errorf("property value label name = %q, want %q", got, want)
		}
	})

	t.Run("uses configured name", func(t *testing.T) {
		t.Setenv("ARTIFACTORY_PROPERTY_VALUE_LABEL_NAME", "team")
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := clients[0].propertyValueLabelName, "team"; got != want {
			t.Errorf("property value label name = %q, want %q", got, want)
		}
	})

	t.Run("rejects invalid name", func(t *testing.T) {
		t.Setenv("ARTIFACTORY_PROPERTY_VALUE_LABEL_NAME", "bad label")
		if _, err := newArtifactoryClients(); err == nil {
			t.Fatal("newArtifactoryClients() succeeded with invalid label name")
		}
	})
}

func TestServerLabelConfiguration(t *testing.T) {
	setMetadataConfig(t)

	t.Run("defaults to Artifactory url", func(t *testing.T) {
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := clients[0].server, "https://artifactory.example.com"; got != want {
			t.Errorf("server = %q, want %q", got, want)
		}
	})

	t.Run("uses configured override", func(t *testing.T) {
		target := defaultTargetConfig()
		target.Server = "production.example.com"
		setTargetConfigs(t, target)
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := clients[0].server, "production.example.com"; got != want {
			t.Errorf("server = %q, want %q", got, want)
		}
	})

	t.Run("uses server override", func(t *testing.T) {
		target := defaultTargetConfig()
		target.Server = "production"
		setTargetConfigs(t, target)
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := clients[0].server, "production"; got != want {
			t.Errorf("server = %q, want %q", got, want)
		}
	})
}

func TestMultiTargetConfiguration(t *testing.T) {
	setMetadataConfig(t)
	firstTarget := defaultTargetConfig()
	firstTarget.Url = "https://one.example.com/"
	firstTarget.Server = "one"
	firstTarget.Token = "token-one"
	firstTarget.MetadataUrl = "https://metadata-one.example.com/{repo}.yaml"
	firstTarget.MetadataGroupKey = "team-one"
	firstTarget.MetadataCacheVersionUrl = "https://metadata-one.example.com/version.json"
	firstTarget.MetadataCacheVersionField = "revision-one"
	firstTarget.MetadataToken = "metadata-token-one"
	secondTarget := defaultTargetConfig()
	secondTarget.Url = "https://two.example.com"
	secondTarget.Server = "two"
	secondTarget.Token = "token-two"
	secondTarget.MetadataUrl = "https://metadata-two.example.com/{repo}.yaml"
	secondTarget.MetadataGroupKey = "team-two"
	setTargetConfigs(t, firstTarget, secondTarget)

	clients, err := newArtifactoryClients()
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 2 {
		t.Fatalf("configured clients = %d, want 2", len(clients))
	}
	if clients[0].baseUrl != "https://one.example.com" || clients[0].server != "one" || clients[0].token != "token-one" {
		t.Errorf("first client config = %#v", clients[0])
	}
	if clients[0].metadataUrlTemplate != firstTarget.MetadataUrl || clients[0].metadataGroupKey != firstTarget.MetadataGroupKey || clients[0].metadataToken != firstTarget.MetadataToken {
		t.Errorf("first repository metadata config = %#v", clients[0])
	}
	if clients[1].baseUrl != "https://two.example.com" || clients[1].server != "two" || clients[1].token != "token-two" {
		t.Errorf("second client config = %#v", clients[1])
	}
	if clients[1].metadataUrlTemplate != secondTarget.MetadataUrl || clients[1].metadataGroupKey != secondTarget.MetadataGroupKey || clients[1].metadataCache.enabled {
		t.Errorf("second repository metadata config = %#v", clients[1])
	}
}

func TestMultiTargetTokenEnvironmentVariable(t *testing.T) {
	setMetadataConfig(t)
	target := defaultTargetConfig()
	target.Server = "one"
	target.TokenEnv = "ARTIFACTORY_TOKEN_ONE"
	setTargetConfigs(t, target)
	t.Setenv("ARTIFACTORY_TOKEN_ONE", "secret-token")

	clients, err := newArtifactoryClients()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := clients[0].token, "secret-token"; got != want {
		t.Errorf("resolved token = %q, want configured environment value", got)
	}
}

func TestMultiTargetTokenEnvironmentVariableMustBeSet(t *testing.T) {
	setMetadataConfig(t)
	target := defaultTargetConfig()
	target.TokenEnv = "MISSING_ARTIFACTORY_TOKEN"
	setTargetConfigs(t, target)
	t.Setenv("MISSING_ARTIFACTORY_TOKEN", "")
	if _, err := newArtifactoryClients(); err == nil {
		t.Fatal("newArtifactoryClients() succeeded with an unset token environment variable")
	}
}

func TestCollectorAggregatesRepositorySizesByMetadataYaml(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization header = %q, want bearer token", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/artifactory/api/storageinfo":
			_, _ = w.Write([]byte(`{"repositoriesSummaryList":[{"repoKey":"repo-one","repoType":"LOCAL","usedSpace":"ignored","usedSpaceInBytes":1536},{"repoKey":"repo-two","repoType":"REMOTE","usedSpaceInBytes":2097152},{"repoKey":"repo-three","repoType":"LOCAL","usedSpaceInBytes":42},{"repoKey":"repo-four","repoType":"LOCAL","usedSpaceInBytes":5120},{"repoKey":"repo-five","repoType":"REMOTE","usedSpaceInBytes":1048576},{"repoKey":"auto-trashcan","repoType":"NA","usedSpaceInBytes":128000},{"repoKey":"jfrog-support-bundle","repoType":"NA","usedSpaceInBytes":64000},{"repoKey":"TOTAL","repoType":"NA","usedSpaceInBytes":3344426}]}`))
		case "/metadata/repo-one.yaml":
			_, _ = io.WriteString(w, "team: blue\n")
		case "/metadata/repo-two.yaml":
			_, _ = io.WriteString(w, "team: blue\n")
		case "/metadata/repo-three.yaml":
			_, _ = io.WriteString(w, "team: green\n")
		case "/metadata/repo-four.yaml":
			_, _ = io.WriteString(w, "team: ''\n")
		case "/metadata/repo-five.yaml":
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &artifactoryClient{
		baseUrl:                 server.URL,
		server:                  "test-server",
		metadataUrlTemplate:     server.URL + "/metadata/{repo}.yaml",
		metadataGroupKey:        "team",
		propertyGroupMetricName: "custom_repo_size_bytes",
		propertyValueLabelName:  "team",
		token:                   "test-token",
		metadataToken:           "test-token",
		http:                    server.Client(),
	}
	secondClient := *client
	secondClient.server = "test-server-two"
	registry := prometheus.NewRegistry()
	if err := registry.Register(newRepositoryCollector([]*artifactoryClient{client, &secondClient})); err != nil {
		t.Fatal(err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	groupedMetricFound := false
	specialRepositoryMetricFound := false
	missingPropertyMetricFound := false
	groupedValuesByServer := make(map[string]map[string]float64)
	specialSizesByServer := make(map[string]map[string]float64)
	missingSizeByServer := make(map[string]float64)
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			serverFound := false
			server := ""
			for _, label := range metric.GetLabel() {
				if label.GetName() == "server" {
					serverFound = true
					server = label.GetValue()
				}
			}
			if !serverFound {
				t.Errorf("metric %q is missing server label", family.GetName())
			}
			switch family.GetName() {
			case "custom_repo_size_bytes":
				groupedMetricFound = true
				if groupedValuesByServer[server] == nil {
					groupedValuesByServer[server] = make(map[string]float64)
				}
				for _, label := range metric.GetLabel() {
					if label.GetName() == "team" {
						groupedValuesByServer[server][label.GetValue()] = metric.GetGauge().GetValue()
					}
				}
			case "artifactory_repository_special_size_bytes":
				specialRepositoryMetricFound = true
				if specialSizesByServer[server] == nil {
					specialSizesByServer[server] = make(map[string]float64)
				}
				for _, label := range metric.GetLabel() {
					if label.GetName() == "repoKey" {
						specialSizesByServer[server][label.GetValue()] = metric.GetGauge().GetValue()
					}
				}
			case "artifactory_repository_size_missing_property_bytes":
				missingPropertyMetricFound = true
				missingSizeByServer[server] = metric.GetGauge().GetValue()
			}
		}
	}
	for _, server := range []string{"test-server", "test-server-two"} {
		gotValues := groupedValuesByServer[server]
		if got, want := gotValues["blue"], 1.5*1024+2*1024*1024; got != want {
			t.Errorf("%s blue group size = %v, want %v", server, got, want)
		}
		if got, want := gotValues["green"], 42.0; got != want {
			t.Errorf("%s green group size = %v, want %v", server, got, want)
		}
		if got, want := missingSizeByServer[server], 5*1024.0+1024*1024; got != want {
			t.Errorf("%s missing-property size = %v, want %v", server, got, want)
		}
		for repositoryKey, want := range map[string]float64{
			"TOTAL":                3344426,
			"auto-trashcan":        128000,
			"jfrog-support-bundle": 64000,
		} {
			if got := specialSizesByServer[server][repositoryKey]; got != want {
				t.Errorf("%s special repository %q size = %v, want %v", server, repositoryKey, got, want)
			}
		}
	}
	if !groupedMetricFound {
		t.Error("property group size metric was not emitted")
	}
	if !missingPropertyMetricFound {
		t.Error("missing-property size metric was not emitted")
	}
	if !specialRepositoryMetricFound {
		t.Error("special repository size metric was not emitted")
	}
}

func TestRepositoryGroupValuesCache(t *testing.T) {
	requestCounts := make(map[string]int)
	revision := "v1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCounts[r.URL.Path]++
		switch r.URL.Path {
		case "/version.json":
			_, _ = io.WriteString(w, `{"revision":"`+revision+`"}`)
		case "/missing.yaml":
			http.NotFound(w, r)
		case "/error.yaml":
			if requestCounts[r.URL.Path] == 1 {
				http.Error(w, "temporary failure", http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, "team: blue\n")
		default:
			_, _ = io.WriteString(w, "team: blue\n")
		}
	}))
	defer server.Close()

	client := &artifactoryClient{
		metadataUrlTemplate:     server.URL + "/{repo}.yaml",
		metadataGroupKey:        "team",
		metadataCacheVersionUrl: server.URL + "/version.json",
		metadataCacheField:      "revision",
		http:                    server.Client(),
		metadataCache:           &repositoryGroupValueCache{enabled: true},
	}
	repositories := []repositorySummary{{Key: "repo", Type: "LOCAL"}, {Key: "missing", Type: "LOCAL"}, {Key: "TOTAL", Type: "NA"}}
	for range 2 {
		values, err := client.repositoryGroupValues(t.Context(), repositories)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := values["repo"], "blue"; got != want {
			t.Errorf("group value = %q, want %q", got, want)
		}
		if _, exists := values["missing"]; exists {
			t.Error("missing metadata unexpectedly had a group value")
		}
	}
	if got, want := requestCounts["/version.json"], 2; got != want {
		t.Errorf("version requests = %d, want %d", got, want)
	}
	for _, path := range []string{"/repo.yaml", "/missing.yaml"} {
		if got, want := requestCounts[path], 1; got != want {
			t.Errorf("%s requests for stable revision = %d, want %d", path, got, want)
		}
	}

	revision = "v2"
	for range 2 {
		if _, err := client.repositoryGroupValues(t.Context(), repositories); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/repo.yaml", "/missing.yaml"} {
		if got, want := requestCounts[path], 2; got != want {
			t.Errorf("%s requests after revision change = %d, want %d", path, got, want)
		}
	}

	revision = "v3"
	refreshRepositories := append(repositories, repositorySummary{Key: "error", Type: "LOCAL"})
	if _, err := client.repositoryGroupValues(t.Context(), refreshRepositories); err == nil {
		t.Fatal("repositoryGroupValues() succeeded after a failed cache refresh")
	}
	if got, want := client.metadataCache.revision, "string:v2"; got != want {
		t.Errorf("revision after failed refresh = %q, want %q", got, want)
	}
	if values, err := client.repositoryGroupValues(t.Context(), refreshRepositories); err != nil {
		t.Fatal(err)
	} else if got, want := values["error"], "blue"; got != want {
		t.Errorf("retried group value = %q, want %q", got, want)
	}
	for path, want := range map[string]int{"/repo.yaml": 4, "/missing.yaml": 4, "/error.yaml": 2} {
		if got := requestCounts[path]; got != want {
			t.Errorf("%s requests after retrying refresh = %d, want %d", path, got, want)
		}
	}
	client.metadataCache.mu.Lock()
	stats := client.metadataCache.stats
	client.metadataCache.mu.Unlock()
	wantStats := repositoryCacheStats{
		repositoriesSeen:      20,
		repositoriesEligible:  14,
		ungroupedRepositories: 5,
		versionChecks:         6,
		hits:                  4,
		misses:                10,
		refreshes:             4,
		metadataFetches:       10,
		errors:                1,
	}
	if stats != wantStats {
		t.Errorf("cache stats = %+v, want %+v", stats, wantStats)
	}
}

func TestRepositoryGroupValuesCacheDisabled(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repo.yaml" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		requestCount++
		_, _ = io.WriteString(w, "team: blue\n")
	}))
	defer server.Close()

	client := &artifactoryClient{
		metadataUrlTemplate: server.URL + "/{repo}.yaml",
		metadataGroupKey:    "team",
		http:                server.Client(),
		metadataCache:       &repositoryGroupValueCache{},
	}
	for range 2 {
		if _, err := client.repositoryGroupValues(t.Context(), []repositorySummary{{Key: "repo", Type: "LOCAL"}}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := requestCount, 2; got != want {
		t.Errorf("metadata requests with cache disabled = %d, want %d", got, want)
	}
	if got, want := client.metadataCache.stats.metadataFetches, uint64(2); got != want {
		t.Errorf("metadata fetch stats with cache disabled = %d, want %d", got, want)
	}
}

func TestRepositoryMetadataAuthentication(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		username string
		password string
		wantAuth string
	}{
		{name: "anonymous"},
		{name: "bearer", token: "version-token", wantAuth: "Bearer version-token"},
		{name: "basic", username: "version-user", password: "version-password", wantAuth: "Basic dmVyc2lvbi11c2VyOnZlcnNpb24tcGFzc3dvcmQ="},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != test.wantAuth {
					t.Errorf("authorization = %q, want %q", got, test.wantAuth)
				}
				if r.URL.Path == "/repo.yaml" {
					_, _ = io.WriteString(w, "team: blue\n")
					return
				}
				_, _ = io.WriteString(w, `{"revision":"v1"}`)
			}))
			defer server.Close()

			client := &artifactoryClient{
				metadataUrlTemplate:     server.URL + "/{repo}.yaml",
				metadataGroupKey:        "team",
				metadataCacheVersionUrl: server.URL + "/version.json",
				metadataCacheField:      "revision",
				metadataToken:           test.token,
				metadataUsername:        test.username,
				metadataPassword:        test.password,
				token:                   "artifactory-token-must-not-be-used",
				username:                "artifactory-user-must-not-be-used",
				password:                "artifactory-password-must-not-be-used",
				http:                    server.Client(),
			}
			if _, err := client.repositoryGroupValue(t.Context(), "repo"); err != nil {
				t.Fatal(err)
			}
			if _, err := client.metadataCacheRevision(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRequestLoggingHandlerLogsDurationAndStats(t *testing.T) {
	var output bytes.Buffer
	originalWriter := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(originalWriter)

	client := &artifactoryClient{
		metadataCache: &repositoryGroupValueCache{
			enabled: true,
			stats: repositoryCacheStats{
				repositoriesSeen:     4,
				repositoriesEligible: 3,
				versionChecks:        2,
				hits:                 1,
				misses:               2,
				refreshes:            1,
				metadataFetches:      2,
			},
		},
	}
	handler := requestLoggingHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), []*artifactoryClient{client})
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))

	for _, expected := range []string{
		`method="GET"`, `path="/metrics"`, "duration=", "cache_enabled_targets=1/1",
		"repositories_seen_total=4", "repositories_eligible_total=3", "cache_version_checks_total=2",
		"cache_hits_total=1", "cache_misses_total=2", "cache_refreshes_total=1",
		"metadata_fetches_total=2", "cache_errors_total=0",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("request log %q does not contain %q", output.String(), expected)
		}
	}
}
