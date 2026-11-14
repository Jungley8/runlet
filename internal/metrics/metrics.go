// Package metrics exposes a tiny Prometheus text endpoint without pulling a
// client library. The label set is bounded by the caller: repo and lane names,
// never run IDs or branches.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Metrics struct {
	mu     sync.Mutex
	values map[string]float64
	kinds  map[string]string
}

func New() *Metrics {
	return &Metrics{
		values: make(map[string]float64),
		kinds:  make(map[string]string),
	}
}

// Add increments a counter.
func (m *Metrics) Add(name string, labels map[string]string, delta float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := name + formatLabels(labels)
	m.kinds[name] = "counter"
	m.values[key] += delta
}

// Set assigns a gauge.
func (m *Metrics) Set(name string, labels map[string]string, v float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := name + formatLabels(labels)
	m.kinds[name] = "gauge"
	m.values[key] = v
}

// Snapshot returns the current values. Tests use it; /metrics writes its own
// sorted text.
func (m *Metrics) Snapshot() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]float64, len(m.values))
	for k, v := range m.values {
		out[k] = v
	}
	return out
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	lines := make([]string, 0, len(m.values)+len(m.kinds))
	seenType := make(map[string]bool)
	names := make([]string, 0, len(m.values))
	for k := range m.values {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		name := k
		if i := strings.IndexByte(k, '{'); i >= 0 {
			name = k[:i]
		}
		if !seenType[name] {
			seenType[name] = true
			lines = append(lines, "# TYPE "+name+" "+m.kinds[name])
		}
		lines = append(lines, k+" "+strconv.FormatFloat(m.values[k], 'g', -1, 64))
	}
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintln(w, strings.Join(lines, "\n"))
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escape(labels[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
