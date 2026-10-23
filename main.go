package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

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

type aqlSearchResult struct {
	Results []aqlRootFolder `json:"results"`
}

type aqlRootFolder struct {
	Repository string        `json:"repo"`
	Properties []aqlProperty `json:"properties"`
}

type aqlProperty struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type artifactoryTargetConfig struct {
	URL      string `json:"url"`
	Server   string `json:"server"`
	Token    string `json:"token"`
	TokenEnv string `json:"tokenEnv"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type artifactoryClient struct {
	baseUrl                 string
	server                  string
	propertyName            string
	propertyGroupMetricName string
	propertyValueLabelName  string
	token                   string
	username                string
	password                string
	http                    *http.Client
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
	mux.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Metrics endpoint scraped from %s", r.RemoteAddr)
		metricsHandler.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":9657"
	}
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

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
	propertyName := strings.TrimSpace(os.Getenv("ARTIFACTORY_PROPERTY_NAME"))
	if propertyName == "" {
		return nil, errors.New("ARTIFACTORY_PROPERTY_NAME is required")
	}
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
			return nil, fmt.Errorf("ARTIFACTORY_TARGETS must be a JSON array: %w", err)
		}
		if len(targets) == 0 {
			return nil, errors.New("ARTIFACTORY_TARGETS must contain at least one target")
		}
	} else {
		targets = []artifactoryTargetConfig{{
			URL:      os.Getenv("ARTIFACTORY_URL"),
			Server:   os.Getenv("ARTIFACTORY_SERVER"),
			Token:    os.Getenv("ARTIFACTORY_TOKEN"),
			Username: os.Getenv("ARTIFACTORY_USERNAME"),
			Password: os.Getenv("ARTIFACTORY_PASSWORD"),
		}}
	}

	clients := make([]*artifactoryClient, 0, len(targets))
	servers := make(map[string]struct{}, len(targets))
	for index, target := range targets {
		baseUrl := strings.TrimRight(strings.TrimSpace(target.URL), "/")
		if baseUrl == "" {
			return nil, fmt.Errorf("Artifactory target %d: url is required", index+1)
		}
		server := strings.TrimSpace(target.Server)
		if server == "" {
			server = baseUrl
		}
		if _, exists := servers[server]; exists {
			return nil, fmt.Errorf("Artifactory target %d: server %q is duplicated", index+1, server)
		}
		servers[server] = struct{}{}
		if (target.Username == "") != (target.Password == "") {
			return nil, fmt.Errorf("Artifactory target %d: username and password must be set together", index+1)
		}
		token := target.Token
		if tokenEnv := strings.TrimSpace(target.TokenEnv); tokenEnv != "" {
			if token != "" {
				return nil, fmt.Errorf("Artifactory target %d: set either token or tokenEnv, not both", index+1)
			}
			var exists bool
			token, exists = os.LookupEnv(tokenEnv)
			if !exists || token == "" {
				return nil, fmt.Errorf("Artifactory target %d: token environment variable %q is unset or empty", index+1, tokenEnv)
			}
		}
		clients = append(clients, &artifactoryClient{
			baseUrl:                 baseUrl,
			server:                  server,
			propertyName:            propertyName,
			propertyGroupMetricName: propertyGroupMetricName,
			propertyValueLabelName:  propertyValueLabelName,
			token:                   token,
			username:                target.Username,
			password:                target.Password,
			http:                    &http.Client{Timeout: 15 * time.Second},
		})
	}
	return clients, nil
}

func (c *artifactoryClient) setAuthentication(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
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

func (c *artifactoryClient) rootFolderPropertyValues(ctx context.Context) (map[string][]string, error) {
	const rootFolderQuery = `items.find({"type":"folder","path":{"$eq":"."},"name":{"$eq":"."}}).include("repo","path","name","property.*")`

	valuesByRepository := make(map[string][]string)
	seenValues := make(map[string]map[string]struct{})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseUrl+"/artifactory/api/search/aql", strings.NewReader(rootFolderQuery))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "text/plain")
	c.setAuthentication(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request root folder properties: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("root folder property query returned http %s", resp.Status)
	}

	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read root folder properties: %w", readErr)
	}
	var result aqlSearchResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode root folder properties: %w", err)
	}

	for _, folder := range result.Results {
		for _, property := range folder.Properties {
			if property.Key != c.propertyName || property.Value == "" {
				continue
			}
			for _, value := range splitPropertyValues(property.Value) {
				if seenValues[folder.Repository] == nil {
					seenValues[folder.Repository] = make(map[string]struct{})
				}
				if _, exists := seenValues[folder.Repository][value]; exists {
					continue
				}
				seenValues[folder.Repository][value] = struct{}{}
				valuesByRepository[folder.Repository] = append(valuesByRepository[folder.Repository], value)
			}
		}
	}
	return valuesByRepository, nil
}

func splitPropertyValues(value string) []string {
	var values []string
	for _, part := range strings.FieldsFunc(value, func(character rune) bool {
		return character == ',' || character == ';'
	}) {
		part = trimNonAlphanumeric(part)
		if part != "" {
			values = append(values, part)
		}
	}
	return values
}

func trimNonAlphanumeric(value string) string {
	return strings.TrimFunc(value, func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character)
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
			"Repository storage grouped by a root-folder property value, in bytes.",
			[]string{"server", propertyValueLabelName}, nil,
		),
		missingPropertySizeDesc: prometheus.NewDesc(
			"artifactory_repository_size_missing_property_bytes",
			"Total storage used by repositories without the configured root-folder property, in bytes.",
			[]string{"server"}, nil,
		),
		specialRepositorySizeDesc: prometheus.NewDesc(
			"artifactory_repository_special_size_bytes",
			"Storage used by an Artifactory storage-summary entry with repoType NA, in bytes.",
			[]string{"server", "repoKey"}, nil,
		),
		upDesc: prometheus.NewDesc(
			"artifactory_up",
			"Whether the most recent Artifactory API requests succeeded.",
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
		var propertyValues map[string][]string
		if err == nil {
			propertyValues, err = client.rootFolderPropertyValues(context.Background())
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
			values := propertyValues[repository.Key]
			if len(values) == 0 {
				missingPropertySize += size
				continue
			}
			for _, value := range values {
				groupSizes[value] += size
			}
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
