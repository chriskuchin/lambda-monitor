package main

import (
	"fmt"
	"testing"
	"time"
)

func Test_processLambdaReports(t *testing.T) {
	now := time.Now()
	fmt.Println(now)
	fmt.Println(now.Truncate(15 * time.Second))
	fmt.Println(now.Truncate(15 * time.Second).Add(15 * time.Second).Sub(time.Now()))
	fmt.Println(time.Now())
}
