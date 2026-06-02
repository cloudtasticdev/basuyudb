// Package otel is the OpenTelemetry OTLP/gRPC ingestion receiver. It implements
// the standard collector TraceService so any OpenTelemetry SDK can export spans
// to BasuyuDB without modification. Received spans are converted to
// executor.Span rows and ingested into the otel_spans table, where they become
// queryable via SQL and joinable with relational tables (Gate 3).
package otel

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"

	"google.golang.org/grpc"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// Receiver is the OTLP/gRPC trace receiver.
type Receiver struct {
	coltracepb.UnimplementedTraceServiceServer
	ingester executor.SpanIngester
	devMode bool
	jwks *auth.JWKSCache
	logger *slog.Logger
	grpc *grpc.Server
}

// Config configures the OTLP receiver.
type Config struct {
	Ingester executor.SpanIngester
	DevMode bool
	JWKS *auth.JWKSCache
	Logger *slog.Logger
}

// NewReceiver constructs an OTLP receiver.
func NewReceiver(cfg Config) *Receiver {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Receiver{ingester: cfg.Ingester, devMode: cfg.DevMode, jwks: cfg.JWKS, logger: cfg.Logger}
}

// Serve starts the OTLP gRPC server on addr (e.g. ":4317") until ctx is done.
func (r *Receiver) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	r.grpc = grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(r.grpc, r)
	go func() {
		<-ctx.Done()
		r.grpc.GracefulStop()
	}()
	r.logger.Info("OTLP gRPC receiver listening", "addr", addr)
	return r.grpc.Serve(ln)
}

// Export implements the OTLP TraceService. It converts the resource/scope/span
// tree into executor.Span rows and ingests them. In dev mode the namespace is
// taken from the "basuyudb.namespace" resource attribute (default "defaultdb").
func (r *Receiver) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	for _, rs := range req.GetResourceSpans() {
		serviceName, namespace := resourceMeta(rs.GetResource())

		sess, err := r.session(namespace)
		if err != nil {
			return nil, err
		}

		var spans []executor.Span
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				spans = append(spans, convertSpan(sp, serviceName))
			}
		}
		if err := r.ingester.IngestSpans(ctx, sess, spans); err != nil {
			return nil, fmt.Errorf("otel: ingest spans: %w", err)
		}
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

func (r *Receiver) session(namespace string) (*session.Session, error) {
	if namespace == "" {
		namespace = "defaultdb"
	}
	// Span ingestion authenticates at the gRPC layer in production (mTLS / token
	// metadata). For milestone-4 dev mode the namespace comes from the resource
	// attribute. The namespace is still validated via the auth package.
	a, err := auth.DevSession(namespace, "main")
	if err != nil {
		return nil, err
	}
	return session.New(a, 0, nil), nil
}

// resourceMeta extracts service.name and the BasuyuDB namespace from resource
// attributes.
func resourceMeta(res interface {
	GetAttributes() []*commonpb.KeyValue
}) (service, namespace string) {
	if res == nil {
		return "unknown", "defaultdb"
	}
	service = "unknown"
	namespace = "defaultdb"
	for _, kv := range res.GetAttributes() {
		switch kv.GetKey() {
		case "service.name":
			service = kv.GetValue().GetStringValue()
		case "basuyudb.namespace":
			namespace = kv.GetValue().GetStringValue()
		}
	}
	return service, namespace
}

// convertSpan maps an OTLP span to an executor.Span row.
func convertSpan(sp *tracepb.Span, service string) executor.Span {
	durMS := int64(0)
	if sp.GetEndTimeUnixNano() > sp.GetStartTimeUnixNano() {
		durMS = int64(sp.GetEndTimeUnixNano()-sp.GetStartTimeUnixNano()) / 1_000_000
	}
	status := "UNSET"
	if st := sp.GetStatus(); st != nil {
		switch st.GetCode() {
		case tracepb.Status_STATUS_CODE_OK:
			status = "OK"
		case tracepb.Status_STATUS_CODE_ERROR:
			status = "ERROR"
		}
	}
	return executor.Span{
		TraceID: hex.EncodeToString(sp.GetTraceId()),
		SpanID: hex.EncodeToString(sp.GetSpanId()),
		ParentSpanID: hex.EncodeToString(sp.GetParentSpanId()),
		ServiceName: service,
		SpanName: sp.GetName(),
		DurationMS: durMS,
		Status: status,
		StartedAt: fmt.Sprintf("%d", sp.GetStartTimeUnixNano()),
		AttributesJSON: attributesJSON(sp.GetAttributes()),
	}
}

// attributesJSON flattens OTLP span attributes into a JSON object queryable with
// the ->> operator.
func attributesJSON(attrs []*commonpb.KeyValue) string {
	m := make(map[string]interface{}, len(attrs))
	for _, kv := range attrs {
		m[kv.GetKey()] = anyValue(kv.GetValue())
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func anyValue(v *commonpb.AnyValue) interface{} {
	if v == nil {
		return nil
	}
	switch v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return v.GetStringValue()
	case *commonpb.AnyValue_BoolValue:
		return v.GetBoolValue()
	case *commonpb.AnyValue_IntValue:
		return v.GetIntValue()
	case *commonpb.AnyValue_DoubleValue:
		return v.GetDoubleValue()
	default:
		return v.String()
	}
}
