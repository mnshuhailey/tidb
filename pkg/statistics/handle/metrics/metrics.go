// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metrics

import (
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// statistics metrics vars
var (
	StatsHealthyGauges []prometheus.Gauge

	DumpHistoricalStatsSuccessCounter prometheus.Counter
	DumpHistoricalStatsFailedCounter  prometheus.Counter
)

func init() {
	InitMetricsVars()
}

// InitMetricsVars init statistics metrics vars.
func InitMetricsVars() {
	StatsHealthyGauges = []prometheus.Gauge{
		metrics.StatsHealthyGauge.WithLabelValues("[0,50)"),           // 0
		metrics.StatsHealthyGauge.WithLabelValues("[50,55)"),          // 1
		metrics.StatsHealthyGauge.WithLabelValues("[55,60)"),          // 2
		metrics.StatsHealthyGauge.WithLabelValues("[60,70)"),          // 3
		metrics.StatsHealthyGauge.WithLabelValues("[70,80)"),          // 4
		metrics.StatsHealthyGauge.WithLabelValues("[80,100)"),         // 5
		metrics.StatsHealthyGauge.WithLabelValues("[100,100]"),        // 6
		metrics.StatsHealthyGauge.WithLabelValues("[0,100]"),          // 7
		metrics.StatsHealthyGauge.WithLabelValues("unneeded analyze"), // 8
	}

	DumpHistoricalStatsSuccessCounter = metrics.HistoricalStatsCounter.WithLabelValues("dump", "success")
	DumpHistoricalStatsFailedCounter = metrics.HistoricalStatsCounter.WithLabelValues("dump", "fail")
}
