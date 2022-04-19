package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"go.elastic.co/apm/module/apmsql"
	_ "go.elastic.co/apm/module/apmsql/sqlite3"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gorilla/mux/otelmux"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/credentials"
)

const (
	serviceName      = "hello-app"
	serviceVersion   = "v1.0.0"
	metricPrefix     = "custom.metric."
	numberOfExecName = metricPrefix + "number.of.exec"
	numberOfExecDesc = "Count the number of executions."
	heapMemoryName   = metricPrefix + "heap.memory"
	heapMemoryDesc   = "Reports heap memory utilization."
)

var (
	tracer trace.Tracer
)

var db *sql.DB

var log = &logrus.Logger{
	Out:   os.Stderr,
	Hooks: make(logrus.LevelHooks),
	Level: logrus.DebugLevel,
	Formatter: &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "@timestamp",
			logrus.FieldKeyLevel: "log.level",
			logrus.FieldKeyMsg:   "message",
			logrus.FieldKeyFunc:  "function.name", // non-ECS
		},
	},
}

func main() {
	ctx := context.Background()
	var err error
	db, err = apmsql.Open("sqlite3", ":memory:")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE stats (name TEXT PRIMARY KEY, count INTEGER);"); err != nil {
		log.Fatal(err)
	}
	// OpenTelemetry agent connectivity data
	endpoint := os.Getenv("EXPORTER_ENDPOINT")
	headers := os.Getenv("EXPORTER_HEADERS")
	headersMap := func(headers string) map[string]string {
		headersMap := make(map[string]string)
		if len(headers) > 0 {
			headerItems := strings.Split(headers, ",")
			for _, headerItem := range headerItems {
				parts := strings.Split(headerItem, "=")
				headersMap[parts[0]] = parts[1]
			}
		}
		return headersMap
	}(headers)

	// Resource to name traces/metrics
	res0urce, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
			semconv.ServiceVersionKey.String(serviceVersion),
			semconv.TelemetrySDKVersionKey.String("v1.4.1"),
			semconv.TelemetrySDKLanguageGo,
		),
	)
	if err != nil {
		log.Fatalf("%s: %v", "failed to create resource", err)
	}

	// Initialize the tracer provider
	initTracer(ctx, endpoint, headersMap, res0urce)
	router := mux.NewRouter()
	router.Use(otelmux.Middleware(serviceName))
	router.HandleFunc("/hello/{name}", hello)
	log.Fatal(http.ListenAndServe(":9000", router))
}

func hello(writer http.ResponseWriter, request *http.Request) {
	vars := mux.Vars(request)
	log.WithField("vars", vars).Info("handling hello request")
	name := vars["name"]
	ctx := request.Context()

	requestCount, err := updateRequestCount(ctx, name)
	if err != nil {
		panic(err)
	}
	buildResponse(writer, requestCount)
}

func updateRequestCount(ctx context.Context, name string) (int, error) {
	_, updateSpan := tracer.Start(ctx, "updateRequestCount")
	defer updateSpan.End()

	if strings.IndexFunc(name, func(r rune) bool { return r >= unicode.MaxASCII }) >= 0 {
		panic("non-ASCII name!")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return -1, err
	}
	row := tx.QueryRowContext(ctx, "SELECT count FROM stats WHERE name=?", name)
	var count int
	switch err := row.Scan(&count); err {
	case nil:
		count++
		if _, err := tx.ExecContext(ctx, "UPDATE stats SET count=? WHERE name=?", count, name); err != nil {
			return -1, err
		}
		log.WithField("name", name).Infof("updated count to %d", count)
	case sql.ErrNoRows:
		count = 1
		if _, err := tx.ExecContext(ctx, "INSERT INTO stats (name, count) VALUES (?, ?)", name, count); err != nil {
			return -1, err
		}
		log.WithField("name", name).Info("initialised count to 1")
	default:
		return -1, err
	}
	return count, tx.Commit()
}

func buildResponse(writer http.ResponseWriter, requestCount int) response {

	writer.WriteHeader(http.StatusOK)
	writer.Header().Add("Content-Type",
		"application/json")

	response := response{fmt.Sprintf("Hello World %d", requestCount)}
	bytes, _ := json.Marshal(response)
	writer.Write(bytes)
	return response
}

type response struct {
	Message string `json:"Message"`
}

func initTracer(ctx context.Context, endpoint string,
	headersMap map[string]string, res0urce *resource.Resource) {

	traceOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithTimeout(5 * time.Second),
	}
	//traceOpts = append(traceOpts, otlptracegrpc.WithHeaders(headersMap))
	traceOpts = append(traceOpts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(&tls.Config{})))
	traceOpts = append(traceOpts, otlptracegrpc.WithEndpoint(endpoint))

	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		log.Fatalf("%s: %v", "failed to create exporter", err)
	}

	otel.SetTracerProvider(sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res0urce),
		sdktrace.WithSpanProcessor(
			sdktrace.NewBatchSpanProcessor(traceExporter)),
	))

	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.Baggage{},
			propagation.TraceContext{},
		),
	)

	tracer = otel.Tracer("io.opentelemetry.traces.hello")

}
