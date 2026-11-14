package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerRendersPrometheusText(t *testing.T) {
	m := New()
	m.Set("runlet_up", nil, 1)
	m.Add("runlet_policy_cancellations_total", map[string]string{"repo": "o/r"}, 2)

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"# TYPE runlet_up gauge",
		"runlet_up 1",
		"# TYPE runlet_policy_cancellations_total counter",
		`runlet_policy_cancellations_total{repo="o/r"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	m := New()
	m.Set("x", map[string]string{"k": `a"b\c`}, 1)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `k="a\"b\\c"`) {
		t.Fatalf("bad escaping: %s", rec.Body.String())
	}
}
