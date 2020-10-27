// Copyright Istio Authors
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
package xds

import (
	"sync"
	"time"

	"google.golang.org/grpc/codes"

	"istio.io/istio/pilot/pkg/model"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/istio/pkg/mcp/status"
	"istio.io/pkg/monitoring"
)

var (
	errTag     = monitoring.MustCreateLabel("err")
	nodeTag    = monitoring.MustCreateLabel("node")
	typeTag    = monitoring.MustCreateLabel("type")
	versionTag = monitoring.MustCreateLabel("version")

	// pilot_total_xds_rejects should be used instead. This is for backwards compatibility
	cdsReject = monitoring.NewGauge(
		"pilot_xds_cds_reject",
		"Pilot rejected CDS configs.",
		monitoring.WithLabels(nodeTag, errTag),
	)

	// pilot_total_xds_rejects should be used instead. This is for backwards compatibility
	edsReject = monitoring.NewGauge(
		"pilot_xds_eds_reject",
		"Pilot rejected EDS.",
		monitoring.WithLabels(nodeTag, errTag),
	)

	// pilot_total_xds_rejects should be used instead. This is for backwards compatibility
	ldsReject = monitoring.NewGauge(
		"pilot_xds_lds_reject",
		"Pilot rejected LDS.",
		monitoring.WithLabels(nodeTag, errTag),
	)

	// pilot_total_xds_rejects should be used instead. This is for backwards compatibility
	rdsReject = monitoring.NewGauge(
		"pilot_xds_rds_reject",
		"Pilot rejected RDS.",
		monitoring.WithLabels(nodeTag, errTag),
	)

	totalXDSRejects = monitoring.NewSum(
		"pilot_total_xds_rejects",
		"Total number of XDS responses from pilot rejected by proxy.",
		monitoring.WithLabels(typeTag),
	)

	// Number of delayed pushes. Currently this happens only when the last push has not been ACKed
	totalDelayedPushes = monitoring.NewSum(
		"pilot_xds_delayed_pushes_total",
		"Total number of XDS pushes that are delayed.",
		monitoring.WithLabels(typeTag),
	)

	// Number of delayed pushes that we pushed prematurely as a failsafe.
	// This indicates that either the failsafe timeout is too aggressive or there is a deadlock
	totalDelayedPushTimeouts = monitoring.NewSum(
		"pilot_xds_delayed_push_timeouts_total",
		"Total number of XDS pushes that are delayed and timed out",
		monitoring.WithLabels(typeTag),
	)

	xdsExpiredNonce = monitoring.NewSum(
		"pilot_xds_expired_nonce",
		"Total number of XDS requests with an expired nonce.",
		monitoring.WithLabels(typeTag),
	)

	monServices = monitoring.NewGauge(
		"pilot_services",
		"Total services known to pilot.",
	)

	// TODO: Update all the resource stats in separate routine
	// virtual services, destination rules, gateways, etc.
	xdsClients = monitoring.NewGauge(
		"pilot_xds",
		"Number of endpoints connected to this pilot using XDS.",
		monitoring.WithLabels(versionTag),
	)
	xdsClientTrackerMutex = &sync.Mutex{}
	xdsClientTracker      = make(map[string]float64)

	xdsResponseWriteTimeouts = monitoring.NewSum(
		"pilot_xds_write_timeout",
		"Pilot XDS response write timeouts.",
	)

	// Covers xds_builderr and xds_senderr for xds in {lds, rds, cds, eds}.
	pushes = monitoring.NewSum(
		"pilot_xds_pushes",
		"Pilot build and send errors for lds, rds, cds and eds.",
		monitoring.WithLabels(typeTag),
	)

	cdsSendErrPushes = pushes.With(typeTag.Value("cds_senderr"))
	edsSendErrPushes = pushes.With(typeTag.Value("eds_senderr"))
	ldsSendErrPushes = pushes.With(typeTag.Value("lds_senderr"))
	rdsSendErrPushes = pushes.With(typeTag.Value("rds_senderr"))

	pushTime = monitoring.NewDistribution(
		"pilot_xds_push_time",
		"Total time in seconds Pilot takes to push lds, rds, cds and eds.",
		[]float64{.01, .1, 1, 3, 5, 10, 20, 30},
		monitoring.WithLabels(typeTag),
	)

	sendTime = monitoring.NewDistribution(
		"pilot_xds_send_time",
		"Total time in seconds Pilot takes to send generated configuration.",
		[]float64{.01, .1, 1, 3, 5, 10, 20, 30},
	)

	// only supported dimension is millis, unfortunately. default to unitdimensionless.
	proxiesQueueTime = monitoring.NewDistribution(
		"pilot_proxy_queue_time",
		"Time in seconds, a proxy is in the push queue before being dequeued.",
		[]float64{.1, 1, 3, 5, 10, 20, 30},
	)

	pushTriggers = monitoring.NewSum(
		"pilot_push_triggers",
		"Total number of times a push was triggered, labeled by reason for the push.",
		monitoring.WithLabels(typeTag),
	)

	// only supported dimension is millis, unfortunately. default to unitdimensionless.
	proxiesConvergeDelay = monitoring.NewDistribution(
		"pilot_proxy_convergence_time",
		"Delay in seconds between config change and a proxy receiving all required configuration.",
		[]float64{.1, .5, 1, 3, 5, 10, 20, 30},
	)

	pushContextErrors = monitoring.NewSum(
		"pilot_xds_push_context_errors",
		"Number of errors (timeouts) initiating push context.",
	)

	totalXDSInternalErrors = monitoring.NewSum(
		"pilot_total_xds_internal_errors",
		"Total number of internal XDS errors in pilot.",
	)

	inboundUpdates = monitoring.NewSum(
		"pilot_inbound_updates",
		"Total number of updates received by pilot.",
		monitoring.WithLabels(typeTag),
	)

	inboundConfigUpdates  = inboundUpdates.With(typeTag.Value("config"))
	inboundEDSUpdates     = inboundUpdates.With(typeTag.Value("eds"))
	inboundServiceUpdates = inboundUpdates.With(typeTag.Value("svc"))
	inboundServiceDeletes = inboundUpdates.With(typeTag.Value("svcdelete"))
)

func recordXDSClients(version string, delta float64) {
	xdsClientTrackerMutex.Lock()
	defer xdsClientTrackerMutex.Unlock()
	xdsClientTracker[version] += delta
	xdsClients.With(versionTag.Value(version)).Record(xdsClientTracker[version])
}

func recordPushTriggers(reasons ...model.TriggerReason) {
	for _, r := range reasons {
		pushTriggers.With(typeTag.Value(string(r))).Increment()
	}
}

func recordSendError(xdsType string, conID string, err error) {
	s, ok := status.FromError(err)
	// Unavailable or canceled code will be sent when a connection is closing down. This is very normal,
	// due to the XDS connection being dropped every 30 minutes, or a pod shutting down.
	isError := s.Code() != codes.Unavailable && s.Code() != codes.Canceled
	if !ok || isError {
		adsLog.Warnf("%s: Send failure %s: %v", xdsType, conID, err)
		// TODO use a single metric with a type tag
		switch xdsType {
		case v3.ListenerType:
			ldsSendErrPushes.Increment()
		case v3.ClusterType:
			cdsSendErrPushes.Increment()
		case v3.EndpointType:
			edsSendErrPushes.Increment()
		case v3.RouteType:
			rdsSendErrPushes.Increment()
		}
	}
}

func incrementXDSRejects(xdsType string, node, errCode string) {
	totalXDSRejects.With(typeTag.Value(v3.GetMetricType(xdsType))).Increment()
	switch xdsType {
	case v3.ListenerType:
		ldsReject.With(nodeTag.Value(node), errTag.Value(errCode)).Increment()
	case v3.ClusterType:
		cdsReject.With(nodeTag.Value(node), errTag.Value(errCode)).Increment()
	case v3.EndpointType:
		edsReject.With(nodeTag.Value(node), errTag.Value(errCode)).Increment()
	case v3.RouteType:
		rdsReject.With(nodeTag.Value(node), errTag.Value(errCode)).Increment()
	}
}

func recordSendTime(duration time.Duration) {
	sendTime.Record(duration.Seconds())
}

func recordPushTime(xdsType string, duration time.Duration) {
	pushTime.With(typeTag.Value(v3.GetMetricType(xdsType))).Record(duration.Seconds())
	pushes.With(typeTag.Value(v3.GetMetricType(xdsType))).Increment()
}

func init() {
	monitoring.MustRegister(
		cdsReject,
		edsReject,
		ldsReject,
		rdsReject,
		xdsExpiredNonce,
		totalXDSRejects,
		monServices,
		xdsClients,
		xdsResponseWriteTimeouts,
		pushes,
		pushTime,
		proxiesConvergeDelay,
		proxiesQueueTime,
		pushContextErrors,
		totalXDSInternalErrors,
		inboundUpdates,
		pushTriggers,
		sendTime,
		totalDelayedPushes,
		totalDelayedPushTimeouts,
	)
}
