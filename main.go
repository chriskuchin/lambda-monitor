package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/urfave/cli/v2"
)

type (
	CLIResult struct {
		Events []LogEvent
	}
	LogEvent struct {
		LogStreamName      string
		Timestamp          int64
		Message            string
		IngestionTimestamp int64
		ID                 string
	}
)

const (
	LAMBDA_LOG_GROUP_PREFIX  = "/aws/lambda/%s"
	LAMBDA_LOG_REPORT_FILTER = "REPORT"
	LAMBDA_LOG_REPORT_PREFIX = "REPORT "
)

var (
	prometheusPort int
	prometheusPath string
	awsRegion      string
	interval       time.Duration

	cfg *session.Session

	kvRegex = regexp.MustCompile("(.+): (.+) (.+)")
	IDRegex = regexp.MustCompile("(.+): (.+)")

	monitorFunctions = map[string]bool{}

	MaxMemoryHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "lambda_monitor",
		Subsystem: "function",
		Name:      "max_memory_mb",
		Help:      "The maximum utilized memory of the function",
		Buckets:   []float64{128, 256, 512, 1024, 1536, 2048, 3072, 4096, 5120, 6144, 7168, 8192, 9216, 10240},
	}, []string{"function"})

	DurationHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "lambda_monitor",
		Subsystem: "function",
		Name:      "duration_ms",
		Help:      "The function run duration",
		Buckets:   []float64{},
	}, []string{"function"})

	BilledDurationHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "lambda_monitor",
		Subsystem: "function",
		Name:      "billed_duration_ms",
		Help:      "The function run billed duration in ms",
		Buckets:   []float64{},
	}, []string{"function"})

	BilledDurationCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "lambda_monitor",
		Subsystem: "function",
		Name:      "billed_duration_total_ms",
		Help:      "The function run billed duration in ms",
	}, []string{"function"})

	InitDurationHistogram = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "lambda_monitor",
		Subsystem: "function",
		Name:      "init_duration_ms",
		Help:      "The function init duration in ms",
		Buckets:   []float64{},
	}, []string{"function"})

	ConfiguredMemoryGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "lambda_monitor",
		Subsystem: "function",
		Name:      "configured_memory_mb",
		Help:      "The function Configured Memory in MB",
	}, []string{"function"})

	InfoGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "lambda_monitor",
		Subsystem: "runtime",
		Name:      "info",
		Help:      "Runtime info for the given function",
	}, []string{"function", "runtime", "memory", "last_modified", "timeout", "revision", "version"})
)

func main() {

	app := cli.App{
		Name:  "lambda-monitor",
		Usage: "Monitor Lambda report logs and export to prometheus",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:        "prometheus-port",
				Usage:       "Port to expose prometheus metrics on",
				EnvVars:     []string{"EXPORTER_PORT"},
				Aliases:     []string{"port"},
				Value:       2112,
				Destination: &prometheusPort,
			},
			&cli.StringFlag{
				Name:        "prometheus-path",
				Usage:       "path to bind the prometeus endopoint to",
				EnvVars:     []string{"EXPORTER_PATH"},
				Aliases:     []string{"path"},
				Value:       "/metrics",
				Destination: &prometheusPath,
			},
			&cli.StringFlag{
				Name:        "aws-region",
				Usage:       "The aws region to connect to",
				EnvVars:     []string{},
				Aliases:     []string{"region"},
				Value:       "us-west-2",
				Destination: &awsRegion,
			},
			&cli.DurationFlag{
				Name:        "interval",
				Usage:       "The scrape Interval",
				EnvVars:     []string{"SCRAPE_INTERVAL"},
				Aliases:     []string{},
				Value:       30 * time.Second,
				Destination: &interval,
			},
		},
		Action: func(c *cli.Context) error {
			cfg = session.New(aws.NewConfig().WithRegion(awsRegion))
			functions := getFunctions()

			for _, fnc := range functions {
				InfoGauge.WithLabelValues(*fnc.FunctionName, *fnc.Runtime, fmt.Sprint(*fnc.MemorySize),
					*fnc.LastModified, fmt.Sprint(*fnc.Timeout), *fnc.RevisionId, *fnc.Version).Set(1)

				if !monitorFunctions[*fnc.FunctionName] {
					go tailLambdaReports(*fnc.FunctionName)
					monitorFunctions[*fnc.FunctionName] = true
				}
			}

			http.Handle(prometheusPath, promhttp.Handler())
			http.HandleFunc("/healthcheck", func(w http.ResponseWriter, r *http.Request) {
				payload := map[string]string{
					"status": "ok",
				}

				result, _ := json.Marshal(payload)

				w.WriteHeader(http.StatusOK)
				w.Write(result)
			})
			http.ListenAndServe(fmt.Sprintf(":%d", prometheusPort), nil)

			return nil
		},
	}

	app.Run(os.Args)
}

func getFunctions() []*lambda.FunctionConfiguration {
	svc := lambda.New(cfg)
	input := &lambda.ListFunctionsInput{}

	result, err := svc.ListFunctions(input)
	if err != nil {
		log.Fatalf("%+v", err)
	}

	return result.Functions
}

func tailLambdaReports(functionName string) {
	for {
		err := processLambdaReports(functionName, interval)
		if err != nil {
			log.Error(err)
			if strings.Contains(err.Error(), "ResourceNotFound") {
				return
			}
		}
		time.Sleep(time.Now().Truncate(interval).Add(interval).Sub(time.Now()))
	}
}

func processLambdaReports(functionName string, window time.Duration) error {
	start := time.Now()
	logGroup := fmt.Sprintf(LAMBDA_LOG_GROUP_PREFIX, functionName)

	logsCommand := exec.Command("aws", "--region", awsRegion, "logs", "filter-log-events",
		"--log-group-name", logGroup, "--start-time", fmt.Sprint(getLogStartTimestamp(start, window)),
		"--end-time", fmt.Sprint(getLogEndTimestamp(start, window)), "--filter-pattern", LAMBDA_LOG_REPORT_FILTER)

	log.Debugf("[%s] cmd: %s", functionName, logsCommand.String())

	output, err := logsCommand.CombinedOutput()
	if err != nil {
		return fmt.Errorf("[%s] Failed to execute CLI command: %+v - %s\n%s", functionName, err, logsCommand.String(), output)
	}

	var result CLIResult
	err = json.Unmarshal(output, &result)
	if err != nil {
		return fmt.Errorf("[%s] failed to parse command result: %+v", functionName, err)
	}

	for _, line := range result.Events {
		kv := strings.Split(strings.TrimPrefix(line.Message, LAMBDA_LOG_REPORT_PREFIX), "\t")

		for _, value := range kv {
			if strings.TrimSpace(value) != "" {
				lambdaInfo := kvRegex.FindStringSubmatch(value)

				if len(lambdaInfo) == 4 {
					handleReportMetric(functionName, lambdaInfo[1], lambdaInfo[2])
				} else {
					requestIDParsed := IDRegex.FindStringSubmatch(value)
					log.Debugf("[%s] %s = %s", functionName, requestIDParsed[1], requestIDParsed[2])
				}
			}
		}
	}

	log.Infof("[%s] Collection Cycle Duration: %v Processed: %d events", functionName, time.Now().Sub(start), len(result.Events))
	return nil
}

func handleReportMetric(function, key, value string) {
	metric, err := strconv.ParseFloat(value, 64)
	if err != nil {
		log.Errorf("[%s] %+v", function, err)
		return
	}

	switch key {
	case "Billed Duration":
		BilledDurationCounter.WithLabelValues(function).Add(metric)
		BilledDurationHistogram.WithLabelValues(function).Observe(metric)
	case "Duration":
		DurationHistogram.WithLabelValues(function).Observe(metric)
	case "Memory Size":
		ConfiguredMemoryGauge.WithLabelValues(function).Set(metric)
	case "Max Memory Used":
		MaxMemoryHistogram.WithLabelValues(function).Observe(metric)
	case "Init Duration":
		InitDurationHistogram.WithLabelValues(function).Observe(metric)
	default:
		log.Warnf("[%s] Unhandled metric key: %s", function, key)
	}
}

func getLogStartTimestamp(now time.Time, window time.Duration) int64 {
	return now.Truncate(window).Add(-window).UTC().UnixNano() / int64(time.Millisecond)
}

func getLogEndTimestamp(now time.Time, window time.Duration) int64 {
	return now.Truncate(window).UTC().UnixNano() / int64(time.Millisecond)
}
