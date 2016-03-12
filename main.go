// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/log"

	"github.com/influxdata/influxdb/models"
)

var (
	webAddress   = flag.String("web.listen-address", ":9122", "Address on which to expose metrics and web interface.")
	metricsPath  = flag.String("web.telemetry-path", "/metrics", "Path under which to expose Prometheus metrics.")
	sampleExpiry = flag.Duration("influxdb.sample-expiry", 5*time.Minute, "How long a sample is valid for.")
	lastPush     = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "influxdb_last_push_timestamp_seconds",
			Help: "Unix timestamp of the last received influxdb metrics push in seconds.",
		},
	)
	invalidChars = regexp.MustCompile("[^a-zA-Z0-9_]")
)

type influxDBSample struct {
	ID        string
	Name      string
	Labels    map[string]string
	Value     float64
	Timestamp time.Time
}

type influxDBCollector struct {
	samples map[string]*influxDBSample
	mu      sync.Mutex
	ch      chan *influxDBSample
}

func newInfluxDBCollector() *influxDBCollector {
	c := &influxDBCollector{
		ch:      make(chan *influxDBSample),
		samples: map[string]*influxDBSample{},
	}
	go c.processSamples()
	return c
}

func (c *influxDBCollector) influxDBPost(w http.ResponseWriter, r *http.Request) {
	lastPush.Set(float64(time.Now().UnixNano()) / 1e9)
	buf, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("error reading body: %s", err), 500)
		return
	}

	precision := "ns"
	if r.FormValue("precision") != "" {
		precision = r.FormValue("precision")
	}
	points, err := models.ParsePointsWithPrecision(buf, time.Now().UTC(), precision)
	if err != nil {
		http.Error(w, fmt.Sprintf("error parsing request: %s", err), 400)
		return
	}

	for _, s := range points {
		for field, v := range s.Fields() {
			var value float64
			switch v := v.(type) {
			case float64:
				value = v
			case int64:
				value = float64(v)
			case bool:
				if v {
					value = 1
				} else {
					value = 0
				}
			default:
				continue
			}

			var name string
			if field == "value" {
				name = s.Name()
			} else {
				name = fmt.Sprintf("%s_%s", s.Name(), field)
			}

			sample := &influxDBSample{
				Name:      invalidChars.ReplaceAllString(name, "_"),
				Timestamp: s.Time(),
				Value:     value,
				Labels:    map[string]string{},
			}
			for k, v := range s.Tags() {
				sample.Labels[invalidChars.ReplaceAllString(k, "_")] = v
			}
			fmt.Printf("%q\n", sample)

			// Calculate a consistent unique ID for the sample.
			labelnames := make([]string, 0, len(sample.Labels))
			for k := range sample.Labels {
				labelnames = append(labelnames, k)
			}
			sort.Strings(labelnames)
			parts := make([]string, 0, len(sample.Labels)*2+1)
			parts = append(parts, name)
			for _, l := range labelnames {
				parts = append(parts, l, sample.Labels[l])
			}
			sample.ID = fmt.Sprintf("%q", parts)

			c.ch <- sample
		}
	}

	// InfluxDB returns a 204 on success.
	http.Error(w, "", 204)
}

func (c *influxDBCollector) processSamples() {
	ticker := time.NewTicker(time.Minute).C
	for {
		select {
		case s := <-c.ch:
			c.mu.Lock()
			c.samples[s.ID] = s
			c.mu.Unlock()

		case <-ticker:
			// Garbage collect expired value lists.
			ageLimit := time.Now().Add(-*sampleExpiry)
			c.mu.Lock()
			for k, sample := range c.samples {
				if ageLimit.After(sample.Timestamp) {
					delete(c.samples, k)
				}
			}
			c.mu.Unlock()
		}
	}
}

// Collect implements prometheus.Collector.
func (c influxDBCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- lastPush

	c.mu.Lock()
	samples := make([]*influxDBSample, 0, len(c.samples))
	for _, sample := range c.samples {
		samples = append(samples, sample)
	}
	c.mu.Unlock()

	ageLimit := time.Now().Add(-*sampleExpiry)
	for _, sample := range samples {
		if ageLimit.After(sample.Timestamp) {
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(sample.Name, "InfluxDB Metric", []string{}, sample.Labels),
			prometheus.UntypedValue,
			sample.Value,
		)
	}
}

// Describe implements prometheus.Collector.
func (c influxDBCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- lastPush.Desc()
}

func main() {
	flag.Parse()

	c := newInfluxDBCollector()
	prometheus.MustRegister(c)

	http.HandleFunc("/write", c.influxDBPost)
	// Some InfluxDB clients try to create a database.
	http.HandleFunc("/query", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"results": []}`)
	})

	http.Handle(*metricsPath, prometheus.Handler())

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
    <head><title>InfluxDB Exporter</title></head>
    <body>
    <h1>InfluxDB Exporter</h1>
    <p><a href="` + *metricsPath + `">Metrics</a></p>
    </body>
    </html>`))
	})

	log.Infof("Starting Server: %s", *webAddress)
	http.ListenAndServe(*webAddress, nil)
}
