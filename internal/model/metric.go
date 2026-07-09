package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// MetricType describes metric semantics.
type MetricType uint8

const (
	MetricTypeGauge MetricType = iota
	MetricTypeCounter
)

func (m MetricType) String() string {
	switch m {
	case MetricTypeGauge:
		return "gauge"
	case MetricTypeCounter:
		return "counter"
	default:
		return "unknown"
	}
}

func ParseMetricType(v string) (MetricType, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "gauge":
		return MetricTypeGauge, nil
	case "counter":
		return MetricTypeCounter, nil
	default:
		return 0, fmt.Errorf("unknown metric type: %q", v)
	}
}

func (m MetricType) MarshalJSON() ([]byte, error) {
	if m == MetricTypeGauge || m == MetricTypeCounter {
		return json.Marshal(m.String())
	}
	return nil, fmt.Errorf("cannot marshal unsupported metric type: %d", m)
}

func (m *MetricType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		parsed, parseErr := ParseMetricType(s)
		if parseErr != nil {
			return parseErr
		}
		*m = parsed
		return nil
	}

	var n uint8
	if err := json.Unmarshal(data, &n); err == nil {
		mt := MetricType(n)
		if mt != MetricTypeGauge && mt != MetricTypeCounter {
			return fmt.Errorf("unknown metric type: %d", n)
		}
		*m = mt
		return nil
	}

	return errors.New("metric type must be string (gauge/counter) or number")
}

// Numeric describes values that can be used in generic metric helpers.
type Numeric interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~float32 | ~float64
}

// TypedValue is a generic helper for typed metric values.
type TypedValue[T Numeric] struct {
	Value T
}

// Metric is a single measurement.
type Metric struct {
	Name      string            `json:"name"`
	Type      MetricType        `json:"type"`
	Value     float64           `json:"value"`
	Timestamp time.Time         `json:"timestamp"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// Batch is a packet of metrics from one agent.
//
// Tenant is set server-side from the authenticated principal, never read from
// the wire (json:"-", and the protobuf Batch has no such field) — a client must
// not be able to choose the tenant its data lands in.
type Batch struct {
	AgentID string   `json:"agent_id"`
	Metrics []Metric `json:"metrics"`
	Tenant  string   `json:"-"`
}

func (m Metric) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("metric name is required")
	}
	if m.Type != MetricTypeGauge && m.Type != MetricTypeCounter {
		return fmt.Errorf("unsupported metric type: %d", m.Type)
	}
	// encoding/json cannot represent NaN/Inf, so the HTTP path rejects them at
	// decode time; protobuf float64 carries them fine, so without this check a
	// gRPC client could smuggle a non-finite value past the identical Validate
	// gate the HTTP path relies on. Reject here to keep both transports uniform.
	if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
		return fmt.Errorf("metric value must be finite, got %v", m.Value)
	}
	if m.Timestamp.IsZero() {
		return errors.New("timestamp is required")
	}
	return nil
}

func (b Batch) Validate() error {
	if strings.TrimSpace(b.AgentID) == "" {
		return errors.New("agent_id is required")
	}
	if len(b.Metrics) == 0 {
		return errors.New("metrics list is empty")
	}
	for i := range b.Metrics {
		if err := b.Metrics[i].Validate(); err != nil {
			return fmt.Errorf("metric[%d]: %w", i, err)
		}
	}
	return nil
}
