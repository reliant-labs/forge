package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeClickHouse(t *testing.T, count int) *Environment {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.Contains(request.URL.Query().Get("query"), "otel_metrics_sum") {
			http.Error(writer, "unexpected table", http.StatusBadRequest)
			return
		}
		_, _ = writer.Write([]byte(`{"data":[{"count":` + string(rune('0'+count)) + `}]}`))
	}))
	t.Cleanup(server.Close)
	environment := &Environment{ProjectName: "shop"}
	environment.SetPort("clickhouse", 8123, strings.TrimPrefix(server.URL, "http://"))
	return environment
}

func TestCheckPrometheus_ClickStackMetricsAbsentIsNotAPass(t *testing.T) {
	result := CheckPrometheus(context.Background(), fakeClickHouse(t, 0))
	if result.Status == StatusPass || !strings.Contains(result.Message, "no metric points from shop") {
		t.Fatalf("want warning naming missing ClickStack metrics, got %s: %s", result.Status, result.Message)
	}
}

func TestCheckPrometheus_ClickStackMetricsPass(t *testing.T) {
	result := CheckPrometheus(context.Background(), fakeClickHouse(t, 4))
	if result.Status != StatusPass || !strings.Contains(result.Message, "4 ClickStack metric point") {
		t.Fatalf("want ClickStack metric pass, got %s: %s", result.Status, result.Message)
	}
}
