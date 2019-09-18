package public

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/deislabs/smi-sdk-go/pkg/apis/split/v1alpha1"
	proto "github.com/golang/protobuf/proto"
	"github.com/linkerd/linkerd2/controller/api/util"
	pb "github.com/linkerd/linkerd2/controller/gen/public"
	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

type resourceResult struct {
	res *pb.StatTable
	err error
}

type k8sStat struct {
	object   metav1.Object
	podStats *podStats
}

type rKey struct {
	Namespace string
	Type      string
	Name      string
}

type tsKey struct {
	Namespace string
	Type      string
	Name      string
	Apex      string
	Leaf      string
	Weight    string
}

const (
	success = "success"
	failure = "failure"

	reqQuery             = "sum(increase(response_total%s[%s])) by (%s, classification, tls)"
	latencyQuantileQuery = "histogram_quantile(%s, sum(irate(response_latency_ms_bucket%s[%s])) by (le, %s))"
	tcpConnectionsQuery  = "sum(tcp_open_connections%s) by (%s)"
	tcpReadBytesQuery    = "sum(increase(tcp_read_bytes_total%s[%s])) by (%s)"
	tcpWriteBytesQuery   = "sum(increase(tcp_write_bytes_total%s[%s])) by (%s)"
)

type podStats struct {
	status string
	inMesh uint64
	total  uint64
	failed uint64
	errors map[string]*pb.PodErrors
}

type trafficSplitStats struct {
	namespace string
	name      string
	apex      string
}

func (s *grpcServer) StatSummary(ctx context.Context, req *pb.StatSummaryRequest) (*pb.StatSummaryResponse, error) {

	// check for well-formed request
	if req.GetSelector().GetResource() == nil {
		return statSummaryError(req, "StatSummary request missing Selector Resource"), nil
	}

	// special case to check for services as outbound only
	if isInvalidServiceRequest(req.Selector, req.GetFromResource()) {
		return statSummaryError(req, "service only supported as a target on 'from' queries, or as a destination on 'to' queries"), nil
	}

	switch req.Outbound.(type) {
	case *pb.StatSummaryRequest_ToResource:
		if req.Outbound.(*pb.StatSummaryRequest_ToResource).ToResource.Type == k8s.All {
			return statSummaryError(req, "resource type 'all' is not supported as a filter"), nil
		}
	case *pb.StatSummaryRequest_FromResource:
		if req.Outbound.(*pb.StatSummaryRequest_FromResource).FromResource.Type == k8s.All {
			return statSummaryError(req, "resource type 'all' is not supported as a filter"), nil
		}
	}

	if isNonK8sResourceQuery(req.GetSelector().GetResource().GetType()) {
		result := s.nonK8sResourceQuery(ctx, req)
		if result.err != nil {
			return nil, util.GRPCError(result.err)
		}

		return statSummaryResponse([]*pb.StatTable{result.res}), nil
	}

	if isTrafficSplitQuery(req.GetSelector().GetResource().GetType()) {
		result := s.trafficSplitResourceQuery(ctx, req)
		if result.err != nil {
			return nil, util.GRPCError(result.err)
		}

		return statSummaryResponse([]*pb.StatTable{result.res}), nil
	}

	if req.Selector.Resource.Type != k8s.All {
		statTables, err := s.k8sResourceQuery(ctx, req)
		if err != nil {
			return nil, util.GRPCError(err)
		}

		// if resource type has no metrics, add an empty row per unit test assertions
		if len(statTables) == 0 {
			statTables = append(statTables, &pb.StatTable{
				Table: &pb.StatTable_PodGroup_{
					PodGroup: &pb.StatTable_PodGroup{},
				},
			})
		}

		return statSummaryResponse(statTables), nil
	}

	// get object stats and metrics for all resources
	statTables, err := s.getStatsForAllResources(ctx, req)
	if err != nil {
		return nil, util.GRPCError(err)
	}

	return statSummaryResponse(statTables), nil
}

func isInvalidServiceRequest(selector *pb.ResourceSelection, fromResource *pb.Resource) bool {
	if fromResource != nil {
		return fromResource.Type == k8s.Service
	}

	return selector.Resource.Type == k8s.Service
}

func statSummaryError(req *pb.StatSummaryRequest, message string) *pb.StatSummaryResponse {
	return &pb.StatSummaryResponse{
		Response: &pb.StatSummaryResponse_Error{
			Error: &pb.ResourceError{
				Resource: req.GetSelector().GetResource(),
				Error:    message,
			},
		},
	}
}

func (s *grpcServer) getKubernetesObjectStats(req *pb.StatSummaryRequest) (map[rKey]k8sStat, error) {
	requestedResource := req.GetSelector().GetResource()
	objects, err := s.k8sAPI.GetObjects(requestedResource.Namespace, requestedResource.Type, requestedResource.Name)
	if err != nil {
		return nil, err
	}

	objectMap := map[rKey]k8sStat{}

	for _, object := range objects {
		metaObj, err := meta.Accessor(object)
		if err != nil {
			return nil, err
		}

		key := rKey{
			Name:      metaObj.GetName(),
			Namespace: metaObj.GetNamespace(),
			Type:      requestedResource.GetType(),
		}

		podStats, err := s.getPodStats(object)
		if err != nil {
			return nil, err
		}

		objectMap[key] = k8sStat{
			object:   metaObj,
			podStats: podStats,
		}
	}
	return objectMap, nil
}

func (s *grpcServer) k8sResourceQuery(ctx context.Context, req *pb.StatSummaryRequest) ([]*pb.StatTable, error) {
	k8sObjects, err := s.getKubernetesObjectStats(req)
	if err != nil {
		return nil, err
	}

	var (
		requestMetrics map[rKey]*pb.BasicStats
		tcpMetrics     map[rKey]*pb.TcpStats
	)
	if !req.SkipStats {
		requestMetrics, tcpMetrics, err = s.getStatMetrics(ctx, req, req.TimeWindow)
		if err != nil {
			return nil, err
		}
	}

	return buildMetricsStatTables(req, k8sObjects, requestMetrics, tcpMetrics), nil
}

func (s *grpcServer) getStatsForAllResources(ctx context.Context, req *pb.StatSummaryRequest) ([]*pb.StatTable, error) {
	var statTables []*pb.StatTable

	// authority stats
	clone := proto.Clone(req).(*pb.StatSummaryRequest)
	clone.Selector.Resource.Type = k8s.Authority
	result := s.nonK8sResourceQuery(ctx, clone)
	if result.err != nil {
		return nil, result.err
	}
	statTables = append(statTables, result.res)

	// traffic split stats
	clone.Selector.Resource.Type = k8s.TrafficSplit
	result = s.trafficSplitResourceQuery(ctx, req)
	if result.err != nil {
		return nil, result.err
	}
	statTables = append(statTables, result.res)

	// object stats from the k8s api server
	k8sObjects := map[rKey]k8sStat{}
	for _, resource := range k8s.StatAllWorkloadResourceTypes {
		clone := proto.Clone(req).(*pb.StatSummaryRequest)
		clone.Selector.Resource.Type = resource
		objStats, err := s.getKubernetesObjectStats(clone)
		if err != nil {
			return nil, util.GRPCError(err)
		}

		// account for types that don't have object stats so that a row will be
		// created for them too, per existing unit test assertions.
		if len(objStats) == 0 {
			k := rKey{
				Type: clone.GetSelector().GetResource().GetType(),
			}
			k8sObjects[k] = k8sStat{}
		}

		for k, v := range objStats {
			k8sObjects[k] = v
		}
	}

	// metrics from prometheus
	var (
		requestMetrics map[rKey]*pb.BasicStats
		tcpMetrics     map[rKey]*pb.TcpStats
		err            error
	)
	if !req.SkipStats {
		requestMetrics, tcpMetrics, err = s.getStatMetrics(ctx, req, req.TimeWindow)
		if err != nil {
			return nil, err
		}
	}

	metricsStatTables := buildMetricsStatTables(req, k8sObjects, requestMetrics, tcpMetrics)
	statTables = append(statTables, metricsStatTables...)
	return statTables, nil
}

func (s *grpcServer) getTrafficSplits(res *pb.Resource) ([]*v1alpha1.TrafficSplit, error) {
	var err error
	var trafficSplits []*v1alpha1.TrafficSplit

	if res.GetNamespace() == "" {
		trafficSplits, err = s.k8sAPI.TS().Lister().List(labels.Everything())
	} else if res.GetName() == "" {
		trafficSplits, err = s.k8sAPI.TS().Lister().TrafficSplits(res.GetNamespace()).List(labels.Everything())
	} else {
		var ts *v1alpha1.TrafficSplit
		ts, err = s.k8sAPI.TS().Lister().TrafficSplits(res.GetNamespace()).Get(res.GetName())
		trafficSplits = []*v1alpha1.TrafficSplit{ts}
	}

	return trafficSplits, err
}

func (s *grpcServer) trafficSplitResourceQuery(ctx context.Context, req *pb.StatSummaryRequest) resourceResult {
	tss, err := s.getTrafficSplits(req.GetSelector().GetResource())

	if err != nil {
		return resourceResult{res: nil, err: err}
	}

	tsBasicStats := make(map[tsKey]*pb.BasicStats)
	rows := make([]*pb.StatTable_PodGroup_Row, 0)

	for _, ts := range tss {
		backends := ts.Spec.Backends

		tsStats := &trafficSplitStats{
			namespace: ts.ObjectMeta.Namespace,
			name:      ts.ObjectMeta.Name,
			apex:      ts.Spec.Service,
		}

		if !req.SkipStats {
			tsBasicStats, err = s.getTrafficSplitMetrics(ctx, req, tsStats, req.TimeWindow)
			if err != nil {
				return resourceResult{res: nil, err: err}
			}
		}

		for _, backend := range backends {
			name := backend.Service
			weight := backend.Weight.String()

			currentLeaf := tsKey{
				Namespace: tsStats.namespace,
				Type:      k8s.TrafficSplit,
				Name:      tsStats.name,
				Apex:      tsStats.apex,
				Leaf:      name,
			}

			trafficSplitStats := &pb.TrafficSplitStats{
				Apex:   tsStats.apex,
				Leaf:   name,
				Weight: weight,
			}

			row := pb.StatTable_PodGroup_Row{
				Resource: &pb.Resource{
					Name:      tsStats.name,
					Namespace: tsStats.namespace,
					Type:      req.GetSelector().GetResource().GetType(),
				},
				TimeWindow: req.TimeWindow,
				Stats:      tsBasicStats[currentLeaf],
				TsStats:    trafficSplitStats,
			}
			rows = append(rows, &row)
		}
	}

	// sort rows before returning in order to have a consistent order for tests
	rows = sortTrafficSplitRows(rows)

	rsp := pb.StatTable{
		Table: &pb.StatTable_PodGroup_{
			PodGroup: &pb.StatTable_PodGroup{
				Rows: rows,
			},
		},
	}

	return resourceResult{res: &rsp, err: nil}
}

func sortTrafficSplitRows(rows []*pb.StatTable_PodGroup_Row) []*pb.StatTable_PodGroup_Row {
	sort.Slice(rows, func(i, j int) bool {
		key1 := rows[i].TsStats.Apex + rows[i].TsStats.Leaf
		key2 := rows[j].TsStats.Apex + rows[j].TsStats.Leaf
		return key1 < key2
	})
	return rows
}

func (s *grpcServer) nonK8sResourceQuery(ctx context.Context, req *pb.StatSummaryRequest) resourceResult {
	var requestMetrics map[rKey]*pb.BasicStats
	if !req.SkipStats {
		var err error
		requestMetrics, _, err = s.getStatMetrics(ctx, req, req.TimeWindow)
		if err != nil {
			return resourceResult{res: nil, err: err}
		}
	}
	rows := make([]*pb.StatTable_PodGroup_Row, 0)

	for rkey, metrics := range requestMetrics {
		rkey.Type = req.GetSelector().GetResource().GetType()

		row := pb.StatTable_PodGroup_Row{
			Resource: &pb.Resource{
				Type:      rkey.Type,
				Namespace: rkey.Namespace,
				Name:      rkey.Name,
			},
			TimeWindow: req.TimeWindow,
			Stats:      metrics,
		}
		rows = append(rows, &row)
	}

	rsp := pb.StatTable{
		Table: &pb.StatTable_PodGroup_{
			PodGroup: &pb.StatTable_PodGroup{
				Rows: rows,
			},
		},
	}
	return resourceResult{res: &rsp, err: nil}
}

func isNonK8sResourceQuery(resourceType string) bool {
	return resourceType == k8s.Authority
}

func isTrafficSplitQuery(resourceType string) bool {
	return resourceType == k8s.TrafficSplit
}

// get the list of objects for which we want to return results
func getResultKeys(
	req *pb.StatSummaryRequest,
	k8sObjects map[rKey]k8sStat,
	metricResults map[rKey]*pb.BasicStats,
) []rKey {
	var keys []rKey
	if req.GetOutbound() == nil || req.GetNone() != nil {
		// if the request doesn't have outbound filtering, return all rows
		for key := range k8sObjects {
			keys = append(keys, key)
		}
	} else {
		// if the request does have outbound filtering,
		// only return rows for which we have stats
		for key := range metricResults {
			keys = append(keys, key)
		}
	}

	return keys
}

func buildRequestLabels(req *pb.StatSummaryRequest) (labels model.LabelSet, labelNames model.LabelNames) {
	// labelNames: the group by in the prometheus query
	// labels: the labels for the resource we want to query for
	switch out := req.Outbound.(type) {
	case *pb.StatSummaryRequest_ToResource:
		labelNames = promGroupByLabelNames(req.Selector.Resource)

		labels = labels.Merge(promDstQueryLabels(out.ToResource))
		labels = labels.Merge(promQueryLabels(req.Selector.Resource))
		labels = labels.Merge(promDirectionLabels("outbound"))

	case *pb.StatSummaryRequest_FromResource:
		labelNames = promDstGroupByLabelNames(req.Selector.Resource)

		labels = labels.Merge(promQueryLabels(out.FromResource))
		labels = labels.Merge(promDstQueryLabels(req.Selector.Resource))
		labels = labels.Merge(promDirectionLabels("outbound"))

	default:
		labelNames = promGroupByLabelNames(req.Selector.Resource)

		labels = labels.Merge(promQueryLabels(req.Selector.Resource))
		labels = labels.Merge(promDirectionLabels("inbound"))
	}

	return
}

func buildTrafficSplitRequestLabels(req *pb.StatSummaryRequest) (labels model.LabelSet, labelNames model.LabelNames) {
	// trafficsplit labels are always direction="outbound" with an optional namespace="value" if the -A flag is not used.
	// if the --from or --to flags were used, we merge an additional ToResource or FromResource label.
	// trafficsplit metrics results are always grouped by dst_service.
	labels = model.LabelSet{
		"direction": model.LabelValue("outbound"),
	}

	if req.Selector.Resource.Namespace != "" {
		labels["namespace"] = model.LabelValue(req.Selector.Resource.Namespace)
	}

	switch out := req.Outbound.(type) {
	case *pb.StatSummaryRequest_ToResource:
		labels = labels.Merge(promDstQueryLabels(out.ToResource))

	case *pb.StatSummaryRequest_FromResource:
		labels = labels.Merge(promQueryLabels(out.FromResource))

	default:
		// no extra labels needed
	}

	groupBy := model.LabelNames{model.LabelName("dst_service")}

	return labels, groupBy
}

func (s *grpcServer) getStatMetrics(ctx context.Context, req *pb.StatSummaryRequest, timeWindow string) (map[rKey]*pb.BasicStats, map[rKey]*pb.TcpStats, error) {
	reqLabels, groupBy := buildRequestLabels(req)
	promQueries := map[promType]string{
		promRequests: reqQuery,
	}

	if req.TcpStats {
		promQueries[promTCPConnections] = tcpConnectionsQuery
		promQueries[promTCPReadBytes] = tcpReadBytesQuery
		promQueries[promTCPWriteBytes] = tcpWriteBytesQuery
	}
	results, err := s.getPrometheusMetrics(ctx, promQueries, latencyQuantileQuery, reqLabels.String(), timeWindow, groupBy.String())
	if err != nil {
		return nil, nil, err
	}

	basicStats := map[rKey]*pb.BasicStats{}
	tcpStats := map[rKey]*pb.TcpStats{}
	if req.GetSelector().GetResource().GetType() == k8s.All {
		for _, t := range k8s.StatAllWorkloadResourceTypes {
			clone := proto.Clone(req).(*pb.StatSummaryRequest)
			clone.Selector.Resource.Type = t
			resourceBasicStats, resourceTCPStats := processPrometheusMetrics(clone, results)
			for resource, stat := range resourceBasicStats {
				basicStats[resource] = stat
			}
			for resource, stat := range resourceTCPStats {
				tcpStats[resource] = stat
			}
		}
	} else {
		basicStats, tcpStats = processPrometheusMetrics(req, results)
	}

	return basicStats, tcpStats, nil
}

func (s *grpcServer) getTrafficSplitMetrics(ctx context.Context, req *pb.StatSummaryRequest, tsStats *trafficSplitStats, timeWindow string) (map[tsKey]*pb.BasicStats, error) {
	tsBasicStats := make(map[tsKey]*pb.BasicStats)
	labels, groupBy := buildTrafficSplitRequestLabels(req)

	apex := tsStats.apex
	namespace := tsStats.namespace
	// TODO: add cluster domain to stringToMatch
	stringToMatch := fmt.Sprintf("%s.%s.svc", apex, namespace)

	reqLabels := generateLabelStringWithRegex(labels, "authority", stringToMatch)

	promQueries := map[promType]string{
		promRequests: reqQuery,
	}

	results, err := s.getPrometheusMetrics(ctx, promQueries, latencyQuantileQuery, reqLabels, timeWindow, groupBy.String())

	if err != nil {
		return nil, err
	}

	basicStats, _ := processPrometheusMetrics(req, results) // we don't need tcpStat info for traffic split

	for rKey, basicStatsVal := range basicStats {
		tsBasicStats[tsKey{
			Namespace: namespace,
			Name:      tsStats.name,
			Type:      req.Selector.Resource.Type,
			Apex:      apex,
			Leaf:      rKey.Name,
		}] = basicStatsVal
	}
	return tsBasicStats, nil
}

func processPrometheusMetrics(req *pb.StatSummaryRequest, results []promResult) (map[rKey]*pb.BasicStats, map[rKey]*pb.TcpStats) {
	basicStats := make(map[rKey]*pb.BasicStats)
	tcpStats := make(map[rKey]*pb.TcpStats)

	for _, result := range results {
		for _, sample := range result.vec {
			kind := req.GetSelector().GetResource().GetType()
			var outboundFrom bool
			if req.Outbound != nil {
				_, outboundFrom = req.Outbound.(*pb.StatSummaryRequest_FromResource)
			}
			resource := metricToKey(kind, outboundFrom, sample.Metric)

			addBasicStats := func() {
				if basicStats[resource] == nil {
					basicStats[resource] = &pb.BasicStats{}
				}
			}
			addTCPStats := func() {
				if tcpStats[resource] == nil {
					tcpStats[resource] = &pb.TcpStats{}
				}
			}

			value := extractSampleValue(sample)

			switch result.prom {
			case promRequests:
				addBasicStats()
				switch string(sample.Metric[model.LabelName("classification")]) {
				case success:
					basicStats[resource].SuccessCount += value
				case failure:
					basicStats[resource].FailureCount += value
				}
			case promLatencyP50:
				addBasicStats()
				basicStats[resource].LatencyMsP50 = value
			case promLatencyP95:
				addBasicStats()
				basicStats[resource].LatencyMsP95 = value
			case promLatencyP99:
				addBasicStats()
				basicStats[resource].LatencyMsP99 = value
			case promTCPConnections:
				addTCPStats()
				tcpStats[resource].OpenConnections = value
			case promTCPReadBytes:
				addTCPStats()
				tcpStats[resource].ReadBytesTotal = value
			case promTCPWriteBytes:
				addTCPStats()
				tcpStats[resource].WriteBytesTotal = value
			}
		}
	}

	return basicStats, tcpStats
}

// metricToKey generates a key which is used to match the metric stats we
// queried from prometheus with the k8s object stats we queried from k8s
func metricToKey(kind string, outboundFrom bool, metric model.Metric) rKey {
	kindLabel := model.LabelName(k8s.KindToStatsLabel(kind, outboundFrom))
	namespaceLabel := model.LabelName(k8s.KindToStatsLabel(k8s.Namespace, outboundFrom))

	key := rKey{
		Type: kind,
		Name: string(metric[kindLabel]),
	}
	if !outboundFrom || (outboundFrom && !isNonK8sResourceQuery(kind)) {
		key.Namespace = string(metric[namespaceLabel])
	}

	return key
}

func (s *grpcServer) getPodStats(obj runtime.Object) (*podStats, error) {
	pods, err := s.k8sAPI.GetPodsFor(obj, true)
	if err != nil {
		return nil, err
	}
	podErrors := make(map[string]*pb.PodErrors)
	meshCount := &podStats{}

	if pod, ok := obj.(*corev1.Pod); ok {
		meshCount.status = k8s.GetPodStatus(*pod)
	}

	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodFailed {
			meshCount.failed++
		} else {
			meshCount.total++
			if k8s.IsMeshed(pod, s.controllerNamespace) {
				meshCount.inMesh++
			}
		}

		errors := checkContainerErrors(pod.Status.ContainerStatuses)
		errors = append(errors, checkContainerErrors(pod.Status.InitContainerStatuses)...)

		if len(errors) > 0 {
			podErrors[pod.Name] = &pb.PodErrors{Errors: errors}
		}
	}
	meshCount.errors = podErrors
	return meshCount, nil
}

func toPodError(container, image, reason, message string) *pb.PodErrors_PodError {
	return &pb.PodErrors_PodError{
		Error: &pb.PodErrors_PodError_Container{
			Container: &pb.PodErrors_PodError_ContainerError{
				Message:   message,
				Container: container,
				Image:     image,
				Reason:    reason,
			},
		},
	}
}

func checkContainerErrors(containerStatuses []corev1.ContainerStatus) []*pb.PodErrors_PodError {
	errors := []*pb.PodErrors_PodError{}
	for _, st := range containerStatuses {
		if !st.Ready {
			if st.State.Waiting != nil {
				errors = append(errors, toPodError(st.Name, st.Image, st.State.Waiting.Reason, st.State.Waiting.Message))
			}

			if st.State.Terminated != nil && (st.State.Terminated.ExitCode != 0 || st.State.Terminated.Signal != 0) {
				errors = append(errors, toPodError(st.Name, st.Image, st.State.Terminated.Reason, st.State.Terminated.Message))
			}

			if st.LastTerminationState.Waiting != nil {
				errors = append(errors, toPodError(st.Name, st.Image, st.LastTerminationState.Waiting.Reason, st.LastTerminationState.Waiting.Message))
			}

			if st.LastTerminationState.Terminated != nil {
				errors = append(errors, toPodError(st.Name, st.Image, st.LastTerminationState.Terminated.Reason, st.LastTerminationState.Terminated.Message))
			}
		}
	}
	return errors
}

func buildMetricsStatTables(req *pb.StatSummaryRequest, k8sObjects map[rKey]k8sStat, requestMetrics map[rKey]*pb.BasicStats, tcpMetrics map[rKey]*pb.TcpStats) []*pb.StatTable {
	var statTables []*pb.StatTable

	rowsByResourceType := map[string][]*pb.StatTable_PodGroup_Row{}
	for _, key := range getResultKeys(req, k8sObjects, requestMetrics) {
		objInfo, ok := k8sObjects[key]
		if !ok {
			continue
		}

		// return an empty row for resource types that don't have object stats
		if objInfo.object == nil {
			rsp := &pb.StatTable{
				Table: &pb.StatTable_PodGroup_{
					PodGroup: &pb.StatTable_PodGroup{},
				},
			}

			statTables = append(statTables, rsp)
			continue
		}

		var tcpStats *pb.TcpStats
		if req.TcpStats {
			tcpStats = tcpMetrics[key]
		}

		var basicStats *pb.BasicStats
		if !reflect.DeepEqual(requestMetrics[key], &pb.BasicStats{}) {
			basicStats = requestMetrics[key]
		}

		k8sResource := objInfo.object
		row := &pb.StatTable_PodGroup_Row{
			Resource: &pb.Resource{
				Name:      k8sResource.GetName(),
				Namespace: k8sResource.GetNamespace(),
				Type:      key.Type,
			},
			TimeWindow: req.TimeWindow,
			Stats:      basicStats,
			TcpStats:   tcpStats,
		}

		podStat := objInfo.podStats
		row.Status = podStat.status
		row.MeshedPodCount = podStat.inMesh
		row.RunningPodCount = podStat.total
		row.FailedPodCount = podStat.failed
		row.ErrorsByPod = podStat.errors

		_, exists := rowsByResourceType[key.Type]
		if !exists {
			rowsByResourceType[key.Type] = []*pb.StatTable_PodGroup_Row{row}
		} else {
			rowsByResourceType[key.Type] = append(rowsByResourceType[key.Type], row)
		}
	}

	for _, rows := range rowsByResourceType {
		statTable := &pb.StatTable{
			Table: &pb.StatTable_PodGroup_{
				PodGroup: &pb.StatTable_PodGroup{
					Rows: rows,
				},
			},
		}
		statTables = append(statTables, statTable)
	}

	return statTables
}

func statSummaryResponse(statTables []*pb.StatTable) *pb.StatSummaryResponse {
	return &pb.StatSummaryResponse{
		Response: &pb.StatSummaryResponse_Ok_{ // https://github.com/golang/protobuf/issues/205
			Ok: &pb.StatSummaryResponse_Ok{
				StatTables: statTables,
			},
		},
	}
}
