/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package slos

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/common/model"
	"k8s.io/klog"

	"k8s.io/perf-tests/clusterloader2/pkg/errors"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
	measurementutil "k8s.io/perf-tests/clusterloader2/pkg/measurement/util"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

const (
	apiResponsivenessPrometheusMeasurementName = "APIResponsivenessPrometheus"

	// Thresholds for API call latency as defined in the official K8s SLO
	// https://github.com/kubernetes/community/blob/master/sig-scalability/slos/api_call_latency.md
	resourceThreshold  time.Duration = 1 * time.Second
	namespaceThreshold time.Duration = 5 * time.Second
	clusterThreshold   time.Duration = 30 * time.Second

	currentAPICallMetricsVersion = "v1"

	filters = `resource!="events", verb!~"WATCH|WATCHLIST|PROXY|CONNECT"`

	// latencyQuery matches description of the API call latency SLI and measure 99th percentaile over 5m windows
	//
	// latencyQuery: %v should be replaced with (1) filters and (2) query window size..
	latencyQuery = "quantile_over_time(0.99, apiserver:apiserver_request_latency_1m:histogram_quantile{%v}[%v])"

	// simpleLatencyQuery measures 99th percentile of API call latency  over given period of time
	// it doesn't match SLI, but is useful in shorter tests, where we don't have enough number of windows to use latencyQuery meaningfully.
	//
	// simpleLatencyQuery: placeholders should be replaced with (1) quantile (2) filters and (3) query window size.
	simpleLatencyQuery = "histogram_quantile(%.2f, sum(rate(apiserver_request_duration_seconds_bucket{%v}[%v])) by (resource,  subresource, verb, scope, le))"

	// countQuery %v should be replaced with (1) filters and (2) query window size.
	countQuery = "sum(increase(apiserver_request_duration_seconds_count{%v}[%v])) by (resource, subresource, scope, verb)"

	countSlowQuery = "sum(rate(apiserver_request_duration_seconds_bucket{%v}[%v])) by (resource, subresource, scope, verb)"

	// exclude all buckets of 1s and shorter
	filterGetAndMutating = `verb!~"WATCH|WATCHLIST|PROXY|CONNECT", le!~"0.\\d+|1"`
	// exclude all buckets below or equal 5s
	filterNamespaceList = `scope!="cluster", verb="LIST", le!~"[01234](.\\d+)?|5"`
	// exclude all buckets below or equal 30s
	filterClusterList = `scope="cluster", verb="LIST", le!~"[12]?[0-9](.\\d+)?|30"`

	latencyWindowSize = 5 * time.Minute

	// Number of metrics with highest latency to print. If the latency exceeeds SLO threshold, a metric is printed regardless.
	topToPrint = 5
)

func init() {
	create := func() measurement.Measurement { return createPrometheusMeasurement(&apiResponsivenessGatherer{}) }
	if err := measurement.Register(apiResponsivenessPrometheusMeasurementName, create); err != nil {
		klog.Fatalf("Cannot register %s: %v", apiResponsivenessPrometheusMeasurementName, err)
	}
}

type apiCallMetric struct {
	Resource    string                        `json:"resource"`
	Subresource string                        `json:"subresource"`
	Verb        string                        `json:"verb"`
	Scope       string                        `json:"scope"`
	Latency     measurementutil.LatencyMetric `json:"latency"`
	Count       int                           `json:"count"`
	SlowCount   int                           `json:"slowCount"`
}

type apiCallMetrics struct {
	metrics map[string]*apiCallMetric
}

type apiResponsivenessGatherer struct{}

func (a *apiResponsivenessGatherer) Gather(executor QueryExecutor, startTime time.Time, config *measurement.MeasurementConfig) (measurement.Summary, error) {
	apiCalls, err := a.gatherAPICalls(executor, startTime, config)
	if err != nil {
		return nil, err
	}

	content, err := util.PrettyPrintJSON(apiCalls.ToPerfData())
	if err != nil {
		return nil, err
	}
	summaryName, err := util.GetStringOrDefault(config.Params, "summaryName", a.String())
	if err != nil {
		return nil, err
	}
	summary := measurement.CreateSummary(summaryName, "json", content)

	allowedSlowCalls, err := util.GetIntOrDefault(config.Params, "allowedSlowCalls", 0)
	if err != nil {
		return nil, err
	}

	badMetrics := a.validateAPICalls(config.Identifier, allowedSlowCalls, apiCalls)
	if len(badMetrics) > 0 {
		err = errors.NewMetricViolationError("top latency metric", fmt.Sprintf("there should be no high-latency requests, but: %v", badMetrics))
	}
	return summary, err
}

func (a *apiResponsivenessGatherer) String() string {
	return apiResponsivenessPrometheusMeasurementName
}

func (a *apiResponsivenessGatherer) IsEnabled(config *measurement.MeasurementConfig) bool {
	return true
}

func (a *apiResponsivenessGatherer) gatherAPICalls(executor QueryExecutor, startTime time.Time, config *measurement.MeasurementConfig) (*apiCallMetrics, error) {
	measurementEnd := time.Now()
	measurementDuration := measurementEnd.Sub(startTime)
	promDuration := measurementutil.ToPrometheusTime(measurementDuration)

	useSimple, err := util.GetBoolOrDefault(config.Params, "useSimpleLatencyQuery", false)
	if err != nil {
		return nil, err
	}

	var latencySamples []*model.Sample
	if useSimple {
		quantiles := []float64{0.5, 0.9, 0.99}
		for _, q := range quantiles {
			query := fmt.Sprintf(simpleLatencyQuery, q, filters, promDuration)
			samples, err := executor.Query(query, measurementEnd)
			if err != nil {
				return nil, err
			}
			// Underlying code assumes presence of 'quantile' label, so adding it manually.
			for _, sample := range samples {
				sample.Metric["quantile"] = model.LabelValue(fmt.Sprintf("%.2f", q))
			}
			latencySamples = append(latencySamples, samples...)
		}
	} else {
		// Latency measurement is based on 5m window aggregation,
		// therefore first 5 minutes of the test should be skipped.
		latencyMeasurementDuration := measurementDuration - latencyWindowSize
		if latencyMeasurementDuration < time.Minute {
			latencyMeasurementDuration = time.Minute
		}
		duration := measurementutil.ToPrometheusTime(latencyMeasurementDuration)

		query := fmt.Sprintf(latencyQuery, filters, duration)
		latencySamples, err = executor.Query(query, measurementEnd)
		if err != nil {
			return nil, err
		}
	}

	query := fmt.Sprintf(countQuery, filters, promDuration)
	countSamples, err := executor.Query(query, measurementEnd)
	if err != nil {
		return nil, err
	}

	allowedSlowCalls, err := util.GetIntOrDefault(config.Params, "allowedSlowCalls", 0)
	if err != nil {
		return nil, err
	}

	countSlowSamples := make([]*model.Sample, 0)
	// TODO(oxddr): remove this guard once it's stable
	if allowedSlowCalls != 0 {
		filters := []string{filterGetAndMutating, filterNamespaceList, filterClusterList}
		for _, filter := range filters {
			query := fmt.Sprintf(countSlowQuery, filter, promDuration)
			samples, err := executor.Query(query, measurementEnd)
			if err != nil {
				return nil, err
			}
			countSlowSamples = append(countSlowSamples, samples...)
		}
	}

	return newFromSamples(latencySamples, countSamples, countSlowSamples)
}

func (a *apiResponsivenessGatherer) validateAPICalls(identifier string, allowedSlowCalls int, metrics *apiCallMetrics) []error {
	badMetrics := make([]error, 0)
	top := topToPrint

	for _, apiCall := range metrics.sorted() {
		var err error
		if err = apiCall.Validate(allowedSlowCalls); err != nil {
			badMetrics = append(badMetrics, err)
		}
		if top > 0 || err != nil {
			top--
			prefix := ""
			if err != nil {
				prefix = "WARNING "
			}
			klog.Infof("%s: %vTop latency metric: %v", identifier, prefix, apiCall)
		}
	}
	return badMetrics
}

func newFromSamples(latencySamples, countSamples, countSlowSamples []*model.Sample) (*apiCallMetrics, error) {
	extractCommon := func(sample *model.Sample) (string, string, string, string) {
		return string(sample.Metric["resource"]), string(sample.Metric["subresource"]), string(sample.Metric["verb"]), string(sample.Metric["scope"])
	}

	m := &apiCallMetrics{metrics: make(map[string]*apiCallMetric)}

	for _, sample := range latencySamples {
		resource, subresource, verb, scope := extractCommon(sample)
		quantile, err := strconv.ParseFloat(string(sample.Metric["quantile"]), 64)
		if err != nil {
			return nil, err
		}

		latency := time.Duration(float64(sample.Value) * float64(time.Second))
		m.SetLatency(resource, subresource, verb, scope, quantile, latency)
	}

	for _, sample := range countSamples {
		resource, subresource, verb, scope := extractCommon(sample)
		count := int(math.Round(float64(sample.Value)))
		m.SetCount(resource, subresource, verb, scope, count)
	}

	for _, sample := range countSlowSamples {
		resource, subresource, verb, scope := extractCommon(sample)
		failedCount := int(math.Round(float64(sample.Value)))
		m.SetSlowCount(resource, subresource, verb, scope, failedCount)
	}

	return m, nil
}

func (m *apiCallMetrics) getAPICall(resource, subresource, verb, scope string) *apiCallMetric {
	key := m.buildKey(resource, subresource, verb, scope)
	call, exists := m.metrics[key]
	if !exists {
		call = &apiCallMetric{
			Resource:    resource,
			Subresource: subresource,
			Verb:        verb,
			Scope:       scope,
		}
		m.metrics[key] = call
	}
	return call
}

func (m *apiCallMetrics) SetLatency(resource, subresource, verb, scope string, quantile float64, latency time.Duration) {
	call := m.getAPICall(resource, subresource, verb, scope)
	call.Latency.SetQuantile(quantile, latency)
}

func (m *apiCallMetrics) SetCount(resource, subresource, verb, scope string, count int) {
	if count == 0 {
		return
	}
	call := m.getAPICall(resource, subresource, verb, scope)
	call.Count = count
}

func (m *apiCallMetrics) SetSlowCount(resource, subresource, verb, scope string, count int) {
	if count == 0 {
		return
	}
	call := m.getAPICall(resource, subresource, verb, scope)
	call.SlowCount = count
}

func (m *apiCallMetrics) ToPerfData() *measurementutil.PerfData {
	perfData := &measurementutil.PerfData{Version: currentAPICallMetricsVersion}
	for _, apicall := range m.sorted() {
		item := measurementutil.DataItem{
			Data: map[string]float64{
				"Perc50": float64(apicall.Latency.Perc50) / 1000000, // us -> ms
				"Perc90": float64(apicall.Latency.Perc90) / 1000000,
				"Perc99": float64(apicall.Latency.Perc99) / 1000000,
			},
			Unit: "ms",
			Labels: map[string]string{
				"Verb":        apicall.Verb,
				"Resource":    apicall.Resource,
				"Subresource": apicall.Subresource,
				"Scope":       apicall.Scope,
				"Count":       fmt.Sprintf("%v", apicall.Count),
				"SlowCount":   fmt.Sprintf("%v", apicall.SlowCount),
			},
		}
		perfData.DataItems = append(perfData.DataItems, item)
	}
	return perfData
}

func (m *apiCallMetrics) sorted() []*apiCallMetric {
	all := make([]*apiCallMetric, 0)
	for _, v := range m.metrics {
		all = append(all, v)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Latency.Perc99 > all[j].Latency.Perc99
	})
	return all
}

func (m *apiCallMetrics) buildKey(resource, subresource, verb, scope string) string {
	return fmt.Sprintf("%s|%s|%s|%s", resource, subresource, verb, scope)
}

func (ap *apiCallMetric) Validate(allowedSlowCalls int) error {
	threshold := ap.getSLOThreshold()
	if err := ap.Latency.VerifyThreshold(threshold); err != nil {
		// TODO(oxddr): remove allowedSlowCalls guard once it's stable
		if allowedSlowCalls > 0 && ap.SlowCount <= allowedSlowCalls {
			return nil
		}
		return fmt.Errorf("got: %+v; expected perc99 <= %v", ap, threshold)
	}
	return nil
}

func (ap *apiCallMetric) getSLOThreshold() time.Duration {
	if ap.Verb != "LIST" {
		return resourceThreshold
	}
	if ap.Scope == "cluster" {
		return clusterThreshold
	}
	return namespaceThreshold
}

func (ap *apiCallMetric) String() string {
	return fmt.Sprintf("%+v; threshold: %v", *ap, ap.getSLOThreshold())
}
