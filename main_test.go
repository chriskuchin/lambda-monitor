package main

import (
	"testing"
	"time"
)

func Test_processLambdaReports(t *testing.T) {
	processLambdaReports("collector-event-table-to-firehose", 30*time.Second)
}
