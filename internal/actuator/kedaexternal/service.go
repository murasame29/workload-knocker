package kedaexternal

import (
	"context"
	"time"

	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// metricName is the single metric of a trigger: 1 while the workload runs, 0 while stopped.
const metricName = "workload_knocker_running"

var _ pb.ExternalScalerServer = (*Hub)(nil)

// target is the workload a trigger asks about, parsed from the scaler metadata.
type target struct {
	env string
	// so is the ScaledObject; ns and workload are the optional metadata overrides.
	so       key
	ns       string
	workload string
}

func parseRef(ref *pb.ScaledObjectRef) (target, error) {
	md := ref.GetScalerMetadata()
	env := md["cluster"]
	if env == "" {
		return target{}, status.Error(codes.InvalidArgument, "scaler metadata \"cluster\" is required")
	}
	return target{
		env:      env,
		so:       key{namespace: ref.GetNamespace(), name: ref.GetName()},
		ns:       md["namespace"],
		workload: md["workload"],
	}, nil
}

// resolve returns the workload key of t: the explicit metadata first, else the discovered ScaledObject.
func (t target) resolve(cs *clusterState) key {
	if t.workload != "" {
		ns := t.ns
		if ns == "" {
			ns = t.so.namespace
		}
		return key{namespace: ns, name: t.workload}
	}
	return cs.aliases[t.so]
}

func (h *Hub) state(t target) (bool, error) {
	running, found, _ := h.lookup(t)
	if !found {
		return false, status.Errorf(codes.NotFound, "no workload state for cluster %q", t.env)
	}
	return running, nil
}

// IsActive reports whether the workload is desired running.
func (h *Hub) IsActive(_ context.Context, ref *pb.ScaledObjectRef) (*pb.IsActiveResponse, error) {
	t, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	running, err := h.state(t)
	if err != nil {
		return nil, err
	}
	return &pb.IsActiveResponse{Result: running}, nil
}

// StreamIsActive pushes the desired state on start, on every change and on a keepalive interval.
func (h *Hub) StreamIsActive(ref *pb.ScaledObjectRef, stream grpc.ServerStreamingServer[pb.IsActiveResponse]) error {
	t, err := parseRef(ref)
	if err != nil {
		return err
	}
	if !h.knownCluster(t.env) {
		return status.Errorf(codes.NotFound, "cluster %q is not served by this actuator", t.env)
	}
	h.streamOpened(t.env)
	defer h.streamClosed(t.env)

	keepalive := time.NewTicker(h.keepalive)
	defer keepalive.Stop()
	var last *bool
	for {
		running, found, changed := h.lookup(t)
		if found && (last == nil || *last != running) {
			if err := stream.Send(&pb.IsActiveResponse{Result: running}); err != nil {
				return err
			}
			last = &running
		}
		select {
		case <-stream.Context().Done():
			return status.FromContextError(stream.Context().Err()).Err()
		case <-h.done:
			return status.Error(codes.Unavailable, "controller is shutting down")
		case <-changed:
		case <-keepalive.C:
			if found {
				if err := stream.Send(&pb.IsActiveResponse{Result: running}); err != nil {
					return err
				}
			}
		}
	}
}

// GetMetricSpec returns the single metric with target 1.
func (h *Hub) GetMetricSpec(_ context.Context, ref *pb.ScaledObjectRef) (*pb.GetMetricSpecResponse, error) {
	if _, err := parseRef(ref); err != nil {
		return nil, err
	}
	return &pb.GetMetricSpecResponse{MetricSpecs: []*pb.MetricSpec{{MetricName: metricName, TargetSize: 1, TargetSizeFloat: 1}}}, nil
}

// GetMetrics returns 1 while the workload is desired running and 0 while stopped.
func (h *Hub) GetMetrics(_ context.Context, req *pb.GetMetricsRequest) (*pb.GetMetricsResponse, error) {
	t, err := parseRef(req.GetScaledObjectRef())
	if err != nil {
		return nil, err
	}
	running, err := h.state(t)
	if err != nil {
		return nil, err
	}
	name := req.GetMetricName()
	if name == "" {
		name = metricName
	}
	var v int64
	if running {
		v = 1
	}
	return &pb.GetMetricsResponse{MetricValues: []*pb.MetricValue{{MetricName: name, MetricValue: v, MetricValueFloat: float64(v)}}}, nil
}
