// Package grpcserver exposes the ingest pipeline and the query store over gRPC,
// alongside the existing HTTP transport. It funnels ingested batches into the
// very same pipeline the HTTP handler uses, so both transports share one store.
package grpcserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"metrics-system/internal/grpcconv"
	"metrics-system/internal/model"
	metricspb "metrics-system/internal/proto/metricspb"
	"metrics-system/internal/server/storage"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Ingester is the pipeline entry point the service needs: a non-blocking enqueue
// that reports backpressure by returning false.
type Ingester interface {
	Ingest(batch model.Batch) bool
}

// Reader is the read side the Query RPC needs.
type Reader interface {
	Query(q storage.Query) ([]model.Metric, error)
}

// Service implements metricspb.MetricsServiceServer.
type Service struct {
	metricspb.UnimplementedMetricsServiceServer

	ingester Ingester
	reader   Reader
	logger   *slog.Logger
}

// NewService wires the service to the pipeline and the store.
func NewService(ingester Ingester, reader Reader, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{ingester: ingester, reader: reader, logger: logger}
}

// Ingest handles a single batch (unary). Saturation is reported as
// ResourceExhausted — the gRPC analogue of the HTTP 503 backpressure signal.
func (s *Service) Ingest(_ context.Context, pb *metricspb.Batch) (*metricspb.IngestAck, error) {
	batch, err := grpcconv.BatchFromProto(pb)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "convert batch: %v", err)
	}
	if err := batch.Validate(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if !s.ingester.Ingest(batch) {
		return nil, status.Error(codes.ResourceExhausted, "pipeline overloaded")
	}
	return &metricspb.IngestAck{Accepted: uint64(len(batch.Metrics))}, nil
}

// IngestStream handles a long-lived bidirectional stream. For every batch it
// receives it enqueues into the pipeline and replies with one ack carrying the
// throttle signal, so a fast client can back off without tearing down the
// stream. A malformed or invalid batch is acked with accepted=0 and the stream
// stays open.
func (s *Service) IngestStream(stream grpc.BidiStreamingServer[metricspb.Batch, metricspb.IngestAck]) error {
	for {
		pb, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil // client closed the send direction: clean end.
		}
		if err != nil {
			return err // transport error or context cancellation.
		}

		ack := &metricspb.IngestAck{}
		switch batch, cerr := grpcconv.BatchFromProto(pb); {
		case cerr != nil:
			s.logger.Warn("grpc ingest stream: malformed batch", "error", cerr)
		default:
			if verr := batch.Validate(); verr != nil {
				s.logger.Warn("grpc ingest stream: invalid batch", "error", verr)
			} else if s.ingester.Ingest(batch) {
				ack.Accepted = uint64(len(batch.Metrics))
			} else {
				ack.Throttled = true
			}
		}

		if err := stream.Send(ack); err != nil {
			return err
		}
	}
}

// Query runs a read and streams the matching metrics back, one per message
// (server streaming).
func (s *Service) Query(pb *metricspb.QueryRequest, stream grpc.ServerStreamingServer[metricspb.Metric]) error {
	q, err := queryFromProto(pb)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "%v", err)
	}
	results, err := s.reader.Query(q)
	if err != nil {
		return status.Errorf(codes.Internal, "query: %v", err)
	}
	for i := range results {
		if err := stream.Send(grpcconv.MetricToProto(results[i])); err != nil {
			return err
		}
	}
	return nil
}

// queryFromProto builds a storage.Query from the protobuf request, resolving the
// aggregator and parsing the step duration.
func queryFromProto(pb *metricspb.QueryRequest) (storage.Query, error) {
	if pb == nil || pb.GetName() == "" {
		return storage.Query{}, errors.New("name is required")
	}
	q := storage.Query{Name: pb.GetName(), Limit: int(pb.GetLimit())}

	if len(pb.GetLabels()) > 0 {
		q.Labels = make(map[string]string, len(pb.GetLabels()))
		for k, v := range pb.GetLabels() {
			q.Labels[k] = v
		}
	}
	if pb.GetFrom() != nil {
		q.From = pb.GetFrom().AsTime()
	}
	if pb.GetTo() != nil {
		q.To = pb.GetTo().AsTime()
	}
	if step := pb.GetStep(); step != "" {
		d, err := time.ParseDuration(step)
		if err != nil {
			return storage.Query{}, err
		}
		q.Step = d
	}
	agg, err := storage.AggregatorByName(pb.GetAgg())
	if err != nil {
		return storage.Query{}, err
	}
	q.Aggregator = agg
	return q, nil
}
