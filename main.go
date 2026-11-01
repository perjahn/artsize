package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type storageInfo struct {
	Repositories []repositorySummary `json:"repositoriesSummaryList"`
}

type repositorySummary struct {
	Key              string      `json:"repoKey"`
	Type             string      `json:"repoType"`
	UsedSpaceInBytes json.Number `json:"usedSpaceInBytes"`
}

type artifactoryTargetConfig struct {
	Url                       string `json:"url"`
	Server                    string `json:"server"`
	Token                     string `json:"token"`
	TokenEnv                  string `json:"tokenEnv"`
	Username                  string `json:"username"`
	Password                  string `json:"password"`
	PasswordEnv               string `json:"passwordEnv"`
	MetadataUrl               string `json:"metadataUrl"`
	MetadataGroupKey          string `json:"metadataGroupKey"`
	MetadataCacheVersionUrl   string `json:"metadataCacheVersionUrl"`
	MetadataCacheVersionField string `json:"metadataCacheVersionField"`
	MetadataToken             string `json:"metadataToken"`
	MetadataTokenEnv          string `json:"metadataTokenEnv"`
	MetadataUsername          string `json:"metadataUsername"`
	MetadataPassword          string `json:"metadataPassword"`
	MetadataPasswordEnv       string `json:"metadataPasswordEnv"`
}

type targetCredentials struct {
	token       string
	tokenEnv    string
	username    string
	password    string
	passwordEnv string
}

type repositoryGroupValueCache struct {
	mu       sync.Mutex
	enabled  bool
	revision string
	values   map[string]string
	stats    repositoryCacheStats
}

type repositoryCacheStats struct {
	repositoriesSeen      uint64
	repositoriesEligible  uint64
	ungroupedRepositories uint64
	versionChecks         uint64
	hits                  uint64
	misses                uint64
	refreshes             uint64
	metadataFetches       uint64
	errors                uint64
}

type artifactoryClient struct {
	baseUrl                 string
	server                  string
	metadataUrlTemplate     string
	metadataGroupKey        string
	metadataCacheVersionUrl string
	metadataCacheField      string
	metadataToken           string
	metadataUsername        string
	metadataPassword        string
	propertyGroupMetricName string
	propertyValueLabelName  string
	token                   string
	username                string
	password                string
	http                    *http.Client
	metadataCache           *repositoryGroupValueCache
}

type repositoryCollector struct {
	clients                   []*artifactoryClient
	sizeDesc                  *prometheus.Desc
	missingPropertySizeDesc   *prometheus.Desc
	specialRepositorySizeDesc *prometheus.Desc
	upDesc                    *prometheus.Desc
	scrapeTimeDesc            *prometheus.Desc
}

var prometheusMetricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var prometheusLabelNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func main() {
	clients, err := newArtifactoryClients()
	if err != nil {
		log.Fatal(err)
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(newRepositoryCollector(clients))
	mux := http.NewServeMux()
	metricsHandler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	mux.Handle("/metrics", metricsHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":9657"
	}
	server := &http.Server{Addr: addr, Handler: requestLoggingHandler(mux, clients), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("Serving metrics on %s/metrics", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func newArtifactoryClients() ([]*artifactoryClient, error) {
	propertyGroupMetricName := strings.TrimSpace(os.Getenv("ARTIFACTORY_PROPERTY_GROUP_SIZE_METRIC_NAME"))
	if propertyGroupMetricName == "" {
		propertyGroupMetricName = "artifactory_property_group_size_bytes"
	}
	if !prometheusMetricNamePattern.MatchString(propertyGroupMetricName) {
		return nil, errors.New("ARTIFACTORY_PROPERTY_GROUP_SIZE_METRIC_NAME must be a valid Prometheus metric name")
	}
	propertyValueLabelName := strings.TrimSpace(os.Getenv("ARTIFACTORY_PROPERTY_VALUE_LABEL_NAME"))
	if propertyValueLabelName == "" {
		propertyValueLabelName = "property_value"
	}
	if !prometheusLabelNamePattern.MatchString(propertyValueLabelName) {
		return nil, errors.New("ARTIFACTORY_PROPERTY_VALUE_LABEL_NAME must be a valid Prometheus label name")
	}

	var targets []artifactoryTargetConfig
	if configuredTargets := strings.TrimSpace(os.Getenv("ARTIFACTORY_TARGETS")); configuredTargets != "" {
		if err := json.Unmarshal([]byte(configuredTargets), &targets); err != nil {
			return nil, fmt.Errorf("ARTIFACTORY_TARGETS must be a json array: %w", err)
		}
		if len(targets) == 0 {
			return nil, errors.New("ARTIFACTORY_TARGETS must contain at least one target")
		}
	} else {
		return nil, errors.New("ARTIFACTORY_TARGETS is required")
	}

	clients := make([]*artifactoryClient, 0, len(targets))
	servers := make(map[string]struct{}, len(targets))
	for index, target := range targets {
		metadataUrlTemplate := strings.TrimSpace(target.MetadataUrl)
		if metadataUrlTemplate == "" || !strings.Contains(metadataUrlTemplate, "{repo}") {
			return nil, fmt.Errorf("artifactory target %d: metadataUrl is required and must contain {repo}", index+1)
		}
		metadataGroupKey := strings.TrimSpace(target.MetadataGroupKey)
		if metadataGroupKey == "" {
			return nil, fmt.Errorf("artifactory target %d: metadataGroupKey is required", index+1)
		}
		metadataCacheVersionUrl := strings.TrimSpace(target.MetadataCacheVersionUrl)
		metadataCacheField := strings.TrimSpace(target.MetadataCacheVersionField)
		if (metadataCacheVersionUrl == "") != (metadataCacheField == "") {
			return nil, fmt.Errorf("artifactory target %d: metadataCacheVersionUrl and metadataCacheVersionField must be set together", index+1)
		}
		metadataCredentials, err := resolveTargetCredentials(index+1, "metadata", targetCredentials{
			token:       target.MetadataToken,
			tokenEnv:    target.MetadataTokenEnv,
			username:    target.MetadataUsername,
			password:    target.MetadataPassword,
			passwordEnv: target.MetadataPasswordEnv,
		})
		if err != nil {
			return nil, err
		}
		baseUrl := strings.TrimRight(strings.TrimSpace(target.Url), "/")
		if baseUrl == "" {
			return nil, fmt.Errorf("artifactory target %d: url is required", index+1)
		}
		server := strings.TrimSpace(target.Server)
		if server == "" {
			server = baseUrl
		}
		if _, exists := servers[server]; exists {
			return nil, fmt.Errorf("artifactory target %d: server %q is duplicated", index+1, server)
		}
		servers[server] = struct{}{}
		credentials, err := resolveTargetCredentials(index+1, "", targetCredentials{
			token:       target.Token,
			tokenEnv:    target.TokenEnv,
			username:    target.Username,
			password:    target.Password,
			passwordEnv: target.PasswordEnv,
		})
		if err != nil {
			return nil, err
		}
		metadataCache := &repositoryGroupValueCache{enabled: metadataCacheVersionUrl != ""}
		clients = append(clients, &artifactoryClient{
			baseUrl:                 baseUrl,
			server:                  server,
			metadataUrlTemplate:     metadataUrlTemplate,
			metadataGroupKey:        metadataGroupKey,
			metadataCacheVersionUrl: metadataCacheVersionUrl,
			metadataCacheField:      metadataCacheField,
			metadataToken:           metadataCredentials.token,
			metadataUsername:        metadataCredentials.username,
			metadataPassword:        metadataCredentials.password,
			propertyGroupMetricName: propertyGroupMetricName,
			propertyValueLabelName:  propertyValueLabelName,
			token:                   credentials.token,
			username:                credentials.username,
			password:                credentials.password,
			http:                    &http.Client{Timeout: 15 * time.Second},
			metadataCache:           metadataCache,
		})
	}
	return clients, nil
}

func resolveTargetCredentials(targetNumber int, prefix string, configured targetCredentials) (targetCredentials, error) {
	tokenField := credentialFieldName(prefix, "token")
	tokenEnvField := credentialFieldName(prefix, "tokenEnv")
	passwordField := credentialFieldName(prefix, "password")
	passwordEnvField := credentialFieldName(prefix, "passwordEnv")

	token, err := resolveCredentialValue(targetNumber, configured.token, configured.tokenEnv, tokenField, tokenEnvField)
	if err != nil {
		return targetCredentials{}, err
	}
	password, err := resolveCredentialValue(targetNumber, configured.password, configured.passwordEnv, passwordField, passwordEnvField)
	if err != nil {
		return targetCredentials{}, err
	}
	if (configured.username == "") != (password == "") {
		return targetCredentials{}, fmt.Errorf("artifactory target %d: %s and %s must be set together", targetNumber, credentialFieldName(prefix, "username"), passwordField)
	}
	return targetCredentials{
		token:    token,
		username: configured.username,
		password: password,
	}, nil
}

func credentialFieldName(prefix, field string) string {
	if prefix == "" {
		return field
	}
	return prefix + strings.ToUpper(field[:1]) + field[1:]
}

func resolveCredentialValue(targetNumber int, value, envName, valueField, envField string) (string, error) {
	envName = strings.TrimSpace(envName)
	if envName == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("artifactory target %d: set either %s or %s, not both", targetNumber, valueField, envField)
	}
	resolved, exists := os.LookupEnv(envName)
	if !exists || resolved == "" {
		return "", fmt.Errorf("artifactory target %d: %s environment variable %q is unset or empty", targetNumber, valueField, envName)
	}
	return resolved, nil
}

func (c *artifactoryClient) setAuthentication(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
}

func (c *artifactoryClient) setMetadataAuthentication(req *http.Request) {
	if c.metadataToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.metadataToken)
	} else if c.metadataUsername != "" {
		req.SetBasicAuth(c.metadataUsername, c.metadataPassword)
	}
}

func (c *artifactoryClient) repositorySizes(ctx context.Context) ([]repositorySummary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseUrl+"/artifactory/api/storageinfo", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	c.setAuthentication(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request storage summary: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("storage summary returned http %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read storage summary: %w", err)
	}
	var summary storageInfo
	if err := json.Unmarshal(body, &summary); err != nil {
		return nil, fmt.Errorf("decode storage summary: %w", err)
	}
	return summary.Repositories, nil
}

func (c *artifactoryClient) repositoryGroupValue(ctx context.Context, repositoryKey string) (string, error) {
	metadataUrl := strings.ReplaceAll(c.metadataUrlTemplate, "{repo}", url.PathEscape(repositoryKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataUrl, nil)
	if err != nil {
		return "", fmt.Errorf("create metadata request for repository %q: %w", repositoryKey, err)
	}
	req.Header.Set("Accept", "application/yaml, text/yaml, */*")
	c.setMetadataAuthentication(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request metadata for repository %q: %w", repositoryKey, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return "", fmt.Errorf("metadata for repository %q returned http %s", repositoryKey, resp.Status)
	}

	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return "", fmt.Errorf("read metadata for repository %q: %w", repositoryKey, readErr)
	}
	var document map[string]any
	if err := yaml.Unmarshal(body, &document); err != nil {
		return "", fmt.Errorf("decode metadata for repository %q: %w", repositoryKey, err)
	}
	value := ""
	if groupValue, exists := document[c.metadataGroupKey]; exists && groupValue != nil {
		value = strings.TrimSpace(fmt.Sprint(groupValue))
	}
	return value, nil
}

func (c *artifactoryClient) metadataCacheRevision(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.metadataCacheVersionUrl, nil)
	if err != nil {
		return "", fmt.Errorf("create metadata cache version request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setMetadataAuthentication(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request metadata cache version: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("metadata cache version returned http %s", resp.Status)
	}
	var document map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&document); err != nil {
		return "", fmt.Errorf("decode metadata cache version: %w", err)
	}
	rawValue, exists := document[c.metadataCacheField]
	if !exists {
		return "", fmt.Errorf("metadata cache version document is missing field %q", c.metadataCacheField)
	}
	decoder := json.NewDecoder(strings.NewReader(string(rawValue)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", fmt.Errorf("decode metadata cache version field %q: %w", c.metadataCacheField, err)
	}
	switch typedValue := value.(type) {
	case string:
		if typedValue == "" {
			return "", fmt.Errorf("metadata cache version field %q must not be empty", c.metadataCacheField)
		}
		return "string:" + typedValue, nil
	case json.Number:
		return "number:" + typedValue.String(), nil
	case bool:
		return fmt.Sprintf("boolean:%t", typedValue), nil
	default:
		return "", fmt.Errorf("metadata cache version field %q must be a string, number, or boolean", c.metadataCacheField)
	}
}

func (c *artifactoryClient) repositoryGroupValues(ctx context.Context, repositories []repositorySummary) (map[string]string, error) {
	valuesByRepository := make(map[string]string, len(repositories))
	eligibleRepositories := uint64(0)
	for _, repository := range repositories {
		if repository.Type != "NA" {
			eligibleRepositories++
		}
	}
	if c.metadataCache == nil || !c.metadataCache.enabled {
		if c.metadataCache != nil {
			c.metadataCache.mu.Lock()
			c.metadataCache.stats.repositoriesSeen += uint64(len(repositories))
			c.metadataCache.stats.repositoriesEligible += eligibleRepositories
			c.metadataCache.mu.Unlock()
		}
		for _, repository := range repositories {
			if repository.Type == "NA" {
				continue
			}
			if c.metadataCache != nil {
				c.metadataCache.mu.Lock()
				c.metadataCache.stats.metadataFetches++
				c.metadataCache.mu.Unlock()
			}
			value, err := c.repositoryGroupValue(ctx, repository.Key)
			if err != nil {
				if c.metadataCache != nil {
					c.metadataCache.mu.Lock()
					c.metadataCache.stats.errors++
					c.metadataCache.mu.Unlock()
				}
				return nil, err
			}
			if value != "" {
				valuesByRepository[repository.Key] = value
			}
		}
		if c.metadataCache != nil {
			c.metadataCache.mu.Lock()
			c.metadataCache.stats.ungroupedRepositories += eligibleRepositories - uint64(len(valuesByRepository))
			c.metadataCache.mu.Unlock()
		}
		return valuesByRepository, nil
	}

	cache := c.metadataCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.stats.repositoriesSeen += uint64(len(repositories))
	cache.stats.repositoriesEligible += eligibleRepositories
	cache.stats.versionChecks++
	revision, err := c.metadataCacheRevision(ctx)
	if err != nil {
		cache.stats.errors++
		return nil, err
	}
	revisionChanged := revision != cache.revision
	groupValues := cache.values
	if revisionChanged {
		cache.stats.refreshes++
		groupValues = make(map[string]string, len(repositories))
	}
	for _, repository := range repositories {
		if repository.Type == "NA" {
			continue
		}
		value, exists := groupValues[repository.Key]
		if !exists {
			cache.stats.misses++
			cache.stats.metadataFetches++
			value, err = c.repositoryGroupValue(ctx, repository.Key)
			if err != nil {
				cache.stats.errors++
				return nil, err
			}
			groupValues[repository.Key] = value
		} else {
			cache.stats.hits++
		}
		if value != "" {
			valuesByRepository[repository.Key] = value
		}
	}
	if revisionChanged {
		cache.values = groupValues
		cache.revision = revision
	}
	cache.stats.ungroupedRepositories += eligibleRepositories - uint64(len(valuesByRepository))
	return valuesByRepository, nil
}

func requestLoggingHandler(next http.Handler, clients []*artifactoryClient) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		defer func() {
			var totals repositoryCacheStats
			enabledTargets := 0
			for _, client := range clients {
				if client.metadataCache == nil {
					continue
				}
				client.metadataCache.mu.Lock()
				if client.metadataCache.enabled {
					enabledTargets++
				}
				totals.repositoriesSeen += client.metadataCache.stats.repositoriesSeen
				totals.repositoriesEligible += client.metadataCache.stats.repositoriesEligible
				totals.ungroupedRepositories += client.metadataCache.stats.ungroupedRepositories
				totals.versionChecks += client.metadataCache.stats.versionChecks
				totals.hits += client.metadataCache.stats.hits
				totals.misses += client.metadataCache.stats.misses
				totals.refreshes += client.metadataCache.stats.refreshes
				totals.metadataFetches += client.metadataCache.stats.metadataFetches
				totals.errors += client.metadataCache.stats.errors
				client.metadataCache.mu.Unlock()
			}
			log.Printf("http request method=%q path=%q remote=%q duration=%s cache_enabled_targets=%d/%d repositories_seen_total=%d repositories_eligible_total=%d repositories_ungrouped_total=%d cache_version_checks_total=%d cache_hits_total=%d cache_misses_total=%d cache_refreshes_total=%d metadata_fetches_total=%d cache_errors_total=%d",
				r.Method, r.URL.Path, r.RemoteAddr, time.Since(started), enabledTargets, len(clients),
				totals.repositoriesSeen, totals.repositoriesEligible, totals.ungroupedRepositories,
				totals.versionChecks, totals.hits, totals.misses, totals.refreshes, totals.metadataFetches, totals.errors)
		}()
		next.ServeHTTP(w, r)
	})
}

func newRepositoryCollector(clients []*artifactoryClient) *repositoryCollector {
	metricName := "artifactory_property_group_size_bytes"
	propertyValueLabelName := "property_value"
	if len(clients) > 0 && clients[0].propertyGroupMetricName != "" {
		metricName = clients[0].propertyGroupMetricName
	}
	if len(clients) > 0 && clients[0].propertyValueLabelName != "" {
		propertyValueLabelName = clients[0].propertyValueLabelName
	}
	return &repositoryCollector{
		clients: clients,
		sizeDesc: prometheus.NewDesc(
			metricName,
			"Repository storage grouped by a value from repository metadata yaml, in bytes.",
			[]string{"server", propertyValueLabelName}, nil,
		),
		missingPropertySizeDesc: prometheus.NewDesc(
			"artifactory_repository_size_missing_property_bytes",
			"Total storage used by repositories without the configured metadata value, in bytes.",
			[]string{"server"}, nil,
		),
		specialRepositorySizeDesc: prometheus.NewDesc(
			"artifactory_repository_special_size_bytes",
			"Storage used by an Artifactory storage-summary entry with repoType NA, in bytes.",
			[]string{"server", "repoKey"}, nil,
		),
		upDesc: prometheus.NewDesc(
			"artifactory_up",
			"Whether the most recent Artifactory and metadata requests succeeded.",
			[]string{"server"}, nil,
		),
		scrapeTimeDesc: prometheus.NewDesc(
			"artifactory_scrape_timestamp_seconds",
			"Unix timestamp of the most recent Artifactory storage summary request.",
			[]string{"server"}, nil,
		),
	}
}

func (c *repositoryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.sizeDesc
	ch <- c.missingPropertySizeDesc
	ch <- c.specialRepositorySizeDesc
	ch <- c.upDesc
	ch <- c.scrapeTimeDesc
}

func (c *repositoryCollector) Collect(ch chan<- prometheus.Metric) {
	for _, client := range c.clients {
		now := time.Now()
		repositories, err := client.repositorySizes(context.Background())
		var groupValuesByRepository map[string]string
		if err == nil {
			groupValuesByRepository, err = client.repositoryGroupValues(context.Background(), repositories)
		}
		up := 1.0
		if err != nil {
			up = 0
			log.Printf("Artifactory scrape failed for server %q: %v", client.server, err)
		}
		ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, up, client.server)
		ch <- prometheus.MustNewConstMetric(c.scrapeTimeDesc, prometheus.GaugeValue, float64(now.Unix()), client.server)
		if err != nil {
			continue
		}

		groupSizes := make(map[string]float64)
		missingPropertySize := 0.0
		specialRepositorySizes := make(map[string]float64)
		for _, repository := range repositories {
			sizeBytes, err := repository.UsedSpaceInBytes.Int64()
			if err != nil || sizeBytes < 0 {
				log.Printf("Skipping repository %q for server %q: invalid usedSpaceInBytes %q", repository.Key, client.server, repository.UsedSpaceInBytes)
				continue
			}
			size := float64(sizeBytes)
			if repository.Type == "NA" {
				specialRepositorySizes[repository.Key] = size
				continue
			}
			value := groupValuesByRepository[repository.Key]
			if value == "" {
				missingPropertySize += size
				continue
			}
			groupSizes[value] += size
		}
		specialRepositoryKeys := make([]string, 0, len(specialRepositorySizes))
		for repositoryKey := range specialRepositorySizes {
			specialRepositoryKeys = append(specialRepositoryKeys, repositoryKey)
		}
		sort.Strings(specialRepositoryKeys)
		for _, repositoryKey := range specialRepositoryKeys {
			ch <- prometheus.MustNewConstMetric(c.specialRepositorySizeDesc, prometheus.GaugeValue, specialRepositorySizes[repositoryKey], client.server, repositoryKey)
		}

		groupValues := make([]string, 0, len(groupSizes))
		for value := range groupSizes {
			groupValues = append(groupValues, value)
		}
		sort.Strings(groupValues)
		for _, value := range groupValues {
			ch <- prometheus.MustNewConstMetric(c.sizeDesc, prometheus.GaugeValue, groupSizes[value], client.server, value)
		}
		ch <- prometheus.MustNewConstMetric(c.missingPropertySizeDesc, prometheus.GaugeValue, missingPropertySize, client.server)
	}
}
