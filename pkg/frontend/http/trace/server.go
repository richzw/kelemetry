// Copyright 2023 The Kelemetry Authors.
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

package trace

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jaegertracing/jaeger/model"
	uiconv "github.com/jaegertracing/jaeger/model/converter/json"
	"github.com/jaegertracing/jaeger/storage/spanstore"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"k8s.io/utils/clock"

	"github.com/kubewharf/kelemetry/pkg/frontend/clusterlist"
	jaegerreader "github.com/kubewharf/kelemetry/pkg/frontend/reader"
	tfconfig "github.com/kubewharf/kelemetry/pkg/frontend/tf/config"
	pkghttp "github.com/kubewharf/kelemetry/pkg/http"
	"github.com/kubewharf/kelemetry/pkg/manager"
	"github.com/kubewharf/kelemetry/pkg/metrics"
	"github.com/kubewharf/kelemetry/pkg/util/shutdown"
)

func init() {
	manager.Global.Provide("trace-server", NewTraceServer)
}

type options struct {
	enable bool
}

func (options *options) Setup(fs *pflag.FlagSet) {
	fs.BoolVar(&options.enable, "trace-server-enable", false, "enable trace server for frontend")
}

func (options *options) EnableFlag() *bool { return &options.enable }

type server struct {
	options          options
	logger           logrus.FieldLogger
	clock            clock.Clock
	server           pkghttp.Server
	metrics          metrics.Client
	spanReader       jaegerreader.Interface
	clusterList      clusterlist.Lister
	transformConfigs tfconfig.Provider

	requestMetric metrics.Metric
}

type requestMetric struct {
	Error metrics.LabeledError
}

func NewTraceServer(
	logger logrus.FieldLogger,
	clock clock.Clock,
	httpServer pkghttp.Server,
	metrics metrics.Client,
	spanReader jaegerreader.Interface,
	clusterList clusterlist.Lister,
	transformConfigs tfconfig.Provider,
) *server {
	return &server{
		logger:           logger,
		clock:            clock,
		server:           httpServer,
		metrics:          metrics,
		spanReader:       spanReader,
		clusterList:      clusterList,
		transformConfigs: transformConfigs,
	}
}

func (server *server) Options() manager.Options {
	return &server.options
}

func (server *server) Init(ctx context.Context) error {
	server.requestMetric = server.metrics.New("redirect_request", &requestMetric{})

	server.server.Routes().GET("/extensions/api/v1/trace", func(ctx *gin.Context) {
		logger := server.logger.WithField("source", ctx.Request.RemoteAddr)
		defer shutdown.RecoverPanic(logger)
		metric := &requestMetric{}
		defer server.requestMetric.DeferCount(server.clock.Now(), metric)

		logger.WithField("query", ctx.Request.URL.RawQuery).Infof("GET /extensions/api/v1/trace %v", ctx.Request.URL.Query())

		if code, err := server.handleTrace(ctx, metric); err != nil {
			logger.WithError(err).Error()
			ctx.Status(code)
			_, _ = ctx.Writer.WriteString(err.Error())
			ctx.Abort()
		}
	})

	return nil
}

func (server *server) Start(ctx context.Context) error { return nil }

func (server *server) Close(ctx context.Context) error { return nil }

func (server *server) handleTrace(ctx *gin.Context, metric *requestMetric) (code int, err error) {
	query := traceQuery{}
	err = ctx.BindQuery(&query)
	if err != nil {
		metric.Error = metrics.MakeLabeledError("InvalidParam")
		return 400, fmt.Errorf("invalid param %w", err)
	}

	trace, code, err := server.findTrace(metric, "tracing (exclusive)", query)
	if err != nil {
		return code, err
	}

	hasLogs := false
	for _, span := range trace.Spans {
		if len(span.Logs) > 0 {
			hasLogs = true
		}
	}
	if !hasLogs && len(trace.Spans) > 0 {
		trace, err = server.spanReader.GetTrace(context.Background(), trace.Spans[0].TraceID)
		if err != nil {
			metric.Error = metrics.MakeLabeledError("TraceError")
			return 500, fmt.Errorf("failed to find trace ids %w", err)
		}
	}

	pruneTrace(trace, query.SpanType)

	uiTrace := uiconv.FromDomain(trace)
	ctx.JSON(200, uiTrace)
	return 0, nil
}

func pruneTrace(trace *model.Trace, spanType string) {
	if len(spanType) == 0 {
		return
	}

	for _, span := range trace.Spans {
		var newLogs []model.Log
		for _, log := range span.Logs {
			_, ok := model.KeyValues(log.Fields).FindByKey(spanType)
			if ok {
				newLogs = append(newLogs, log)
			}
		}
		span.Logs = newLogs
	}
}

type traceQuery struct {
	Cluster   string `form:"cluster"`
	Resource  string `form:"resource"`
	Namespace string `form:"namespace"`
	Name      string `form:"name"`
	Ts        string `form:"ts"`
	SpanType  string `form:"span_type"`
}

func (server *server) findTrace(metric *requestMetric, serviceName string, query traceQuery) (trace *model.Trace, code int, err error) {
	cluster := query.Cluster
	resource := query.Resource
	namespace := query.Namespace
	name := query.Name

	if len(cluster) == 0 || len(resource) == 0 || len(name) == 0 {
		metric.Error = metrics.MakeLabeledError("EmptyParam")
		return nil, 400, fmt.Errorf("cluster or resource or name is empty")
	}

	var hasCluster bool
	for _, knownCluster := range server.clusterList.List() {
		if strings.EqualFold(strings.ToLower(knownCluster), strings.ToLower(cluster)) {
			hasCluster = true
		}
	}
	if !hasCluster {
		metric.Error = metrics.MakeLabeledError("UnknownCluster")
		return nil, 404, fmt.Errorf("cluster %s not supported now", cluster)
	}

	timestamp, err := time.Parse(time.RFC3339, query.Ts)
	if err != nil {
		metric.Error = metrics.MakeLabeledError("InvalidTimestamp")
		return nil, 400, fmt.Errorf("invalid timestamp for ts param %w", err)
	}

	tags := map[string]string{
		"resource": resource,
		"name":     name,
	}
	if namespace != "" {
		tags["namespace"] = namespace
	}

	parameters := &spanstore.TraceQueryParameters{
		ServiceName:   serviceName,
		OperationName: cluster,
		Tags:          tags,
		StartTimeMin:  timestamp.Truncate(time.Minute * 30),
		StartTimeMax:  timestamp.Truncate(time.Minute * 30).Add(time.Minute * 30),
	}
	traces, err := server.spanReader.FindTraces(context.Background(), parameters)
	if err != nil {
		metric.Error = metrics.MakeLabeledError("TraceError")
		return nil, 500, fmt.Errorf("failed to find trace ids %w", err)
	}

	if len(traces) > 1 {
		metric.Error = metrics.MakeLabeledError("MultiTraceMatch")
		return nil, 500, fmt.Errorf("trace ids match query length is %d, not 1", len(traces))
	}
	if len(traces) == 0 {
		metric.Error = metrics.MakeLabeledError("NoTraceMatch")
		return nil, 404, fmt.Errorf("could not find trace ids that match query")
	}
	return traces[0], 200, nil
}
