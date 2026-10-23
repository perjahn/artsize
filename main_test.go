package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestSplitPropertyValues(t *testing.T) {
	values := splitPropertyValues(" --blue!! ; (green), blue ; !!! ")
	want := []string{"blue", "green", "blue"}
	if len(values) != len(want) {
		t.Fatalf("splitPropertyValues() = %v, want %v", values, want)
	}
	for index := range want {
		if values[index] != want[index] {
			t.Errorf("splitPropertyValues() = %v, want %v", values, want)
			break
		}
	}
}

func TestPropertyGroupMetricNameConfiguration(t *testing.T) {
	t.Setenv("ARTIFACTORY_URL", "https://artifactory.example.com")
	t.Setenv("ARTIFACTORY_PROPERTY_NAME", "team")
	t.Setenv("ARTIFACTORY_TARGETS", "")
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
	t.Setenv("ARTIFACTORY_URL", "https://artifactory.example.com")
	t.Setenv("ARTIFACTORY_PROPERTY_NAME", "team")
	t.Setenv("ARTIFACTORY_TARGETS", "")

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
	t.Setenv("ARTIFACTORY_URL", "https://artifactory.example.com/")
	t.Setenv("ARTIFACTORY_PROPERTY_NAME", "team")
	t.Setenv("ARTIFACTORY_TARGETS", "")
	t.Setenv("ARTIFACTORY_SERVER", "")

	t.Run("defaults to Artifactory URL", func(t *testing.T) {
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := clients[0].server, "https://artifactory.example.com"; got != want {
			t.Errorf("server = %q, want %q", got, want)
		}
	})

	t.Run("uses configured override", func(t *testing.T) {
		t.Setenv("ARTIFACTORY_SERVER", "production.example.com")
		clients, err := newArtifactoryClients()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := clients[0].server, "production.example.com"; got != want {
			t.Errorf("server = %q, want %q", got, want)
		}
	})

	t.Run("uses server override", func(t *testing.T) {
		t.Setenv("ARTIFACTORY_SERVER", "production")
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
	t.Setenv("ARTIFACTORY_PROPERTY_NAME", "team")
	t.Setenv("ARTIFACTORY_TARGETS", `[{"url":"https://one.example.com/","server":"one","token":"token-one"},{"url":"https://two.example.com","server":"two","token":"token-two"}]`)

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
	if clients[1].baseUrl != "https://two.example.com" || clients[1].server != "two" || clients[1].token != "token-two" {
		t.Errorf("second client config = %#v", clients[1])
	}
}

func TestMultiTargetTokenEnvironmentVariable(t *testing.T) {
	t.Setenv("ARTIFACTORY_PROPERTY_NAME", "team")
	t.Setenv("ARTIFACTORY_TARGETS", `[{"url":"https://one.example.com","server":"one","tokenEnv":"ARTIFACTORY_TOKEN_ONE"}]`)
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
	t.Setenv("ARTIFACTORY_PROPERTY_NAME", "team")
	t.Setenv("ARTIFACTORY_TARGETS", `[{"url":"https://one.example.com","tokenEnv":"MISSING_ARTIFACTORY_TOKEN"}]`)
	t.Setenv("MISSING_ARTIFACTORY_TOKEN", "")
	if _, err := newArtifactoryClients(); err == nil {
		t.Fatal("newArtifactoryClients() succeeded with an unset token environment variable")
	}
}

func TestCollectorAggregatesRepositorySizesByRootProperty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization header = %q, want bearer token", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/artifactory/api/storageinfo":
			_, _ = w.Write([]byte(`{"repositoriesSummaryList":[{"repoKey":"repo-one","repoType":"LOCAL","usedSpace":"ignored","usedSpaceInBytes":1536},{"repoKey":"repo-two","repoType":"REMOTE","usedSpaceInBytes":2097152},{"repoKey":"repo-three","repoType":"LOCAL","usedSpaceInBytes":42},{"repoKey":"repo-four","repoType":"LOCAL","usedSpaceInBytes":5120},{"repoKey":"repo-five","repoType":"REMOTE","usedSpaceInBytes":1048576},{"repoKey":"auto-trashcan","repoType":"NA","usedSpaceInBytes":128000},{"repoKey":"jfrog-support-bundle","repoType":"NA","usedSpaceInBytes":64000},{"repoKey":"TOTAL","repoType":"NA","usedSpaceInBytes":3344426}]}`))
		case "/artifactory/api/search/aql":
			if r.Method != http.MethodPost {
				t.Errorf("AQL method = %q, want POST", r.Method)
			}
			if got := r.Header.Get("Content-Type"); got != "text/plain" {
				t.Errorf("AQL content type = %q, want text/plain", got)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading AQL query: %v", err)
			}
			query := string(body)
			for _, expected := range []string{`"type":"folder"`, `"path":{"$eq":"."}`, `"name":{"$eq":"."}`, `"repo","path","name","property.*"`} {
				if !strings.Contains(query, expected) {
					t.Errorf("AQL query %q does not contain %q", body, expected)
				}
			}
			if strings.Contains(query, ".offset(") || strings.Contains(query, ".limit(") {
				t.Errorf("AQL query should not be paginated: %q", query)
			}
			_, _ = w.Write([]byte(`{"results":[{"repo":"repo-one","properties":[{"key":"team","value":"--blue!!; amber,red;blue"}]},{"repo":"repo-two","properties":[{"key":"team","value":"blue"}]},{"repo":"repo-three","properties":[{"key":"team","value":"(green)"}]},{"repo":"repo-four","properties":[{"key":"team","value":"---!!"}]},{"repo":"repo-five","properties":[]}],"range":{"total":5}}`))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &artifactoryClient{
		baseUrl:                 server.URL,
		server:                  "test-server",
		propertyName:            "team",
		propertyGroupMetricName: "custom_repo_size_bytes",
		propertyValueLabelName:  "team",
		token:                   "test-token",
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
		for _, value := range []string{"amber", "red"} {
			if got, want := gotValues[value], 1.5*1024; got != want {
				t.Errorf("%s %s group size = %v, want %v", server, value, got, want)
			}
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
