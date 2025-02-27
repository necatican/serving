/*
Copyright 2021 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package queue

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/atomic"
	"k8s.io/apimachinery/pkg/util/wait"
	pkglogging "knative.dev/pkg/logging"
	ltesting "knative.dev/pkg/logging/testing"

	network "knative.dev/networking/pkg"
)

func TestConcurrencyStateHandler(t *testing.T) {
	paused := atomic.NewInt64(0)
	resumed := atomic.NewInt64(0)

	handler := func(w http.ResponseWriter, r *http.Request) {}
	logger := ltesting.TestLogger(t)
	h := ConcurrencyStateHandler(logger, http.HandlerFunc(handler), func() { paused.Inc() }, func() { resumed.Inc() })

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://target", nil))
	if got, want := pollFor(paused, 1), int64(1); got != want {
		t.Errorf("Pause was called %d times, want %d times", got, want)
	}

	if got, want := pollFor(resumed, 1), int64(1); got != want {
		t.Errorf("Resume was called %d times, want %d times", got, want)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://target", nil))
	if got, want := pollFor(paused, 2), int64(2); got != want {
		t.Errorf("Pause was called %d times, want %d times", got, want)
	}

	if got, want := pollFor(resumed, 2), int64(2); got != want {
		t.Errorf("Resume was called %d times, want %d times", got, want)
	}
}

func TestConcurrencyStateHandlerParallelSubsumed(t *testing.T) {
	paused := atomic.NewInt64(0)
	resumed := atomic.NewInt64(0)

	req1 := make(chan struct{})
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("req") == "1" {
			req1 <- struct{}{} // to know it's here.
			req1 <- struct{}{} // to make it wait.
		}
	}
	logger := ltesting.TestLogger(t)
	h := ConcurrencyStateHandler(logger, http.HandlerFunc(handler), func() { paused.Inc() }, func() { resumed.Inc() })

	go func() {
		defer func() { req1 <- struct{}{} }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://target?req=1", nil))
	}()

	<-req1 // Wait for req1 to arrive.

	// Send a second request, which can pass through.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://target", nil))

	<-req1 // Allow req1 to pass.
	<-req1 // Wait for req1 to finish.

	if got, want := pollFor(paused, 1), int64(1); got != want {
		t.Errorf("Pause was called %d times, want %d times", got, want)
	}

	if got, want := pollFor(resumed, 1), int64(1); got != want {
		t.Errorf("Resume was called %d times, want %d times", got, want)
	}
}

func TestConcurrencyStateHandlerParallelOverlapping(t *testing.T) {
	paused := atomic.NewInt64(0)
	resumed := atomic.NewInt64(0)

	req1 := make(chan struct{})
	req2 := make(chan struct{})
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("req") == "1" {
			req1 <- struct{}{} // to know it's here.
			req1 <- struct{}{} // to make it wait.
		} else {
			req2 <- struct{}{} // to know it's here.
			req2 <- struct{}{} // to make it wait.
		}
	}
	logger := ltesting.TestLogger(t)
	h := ConcurrencyStateHandler(logger, http.HandlerFunc(handler), func() { paused.Inc() }, func() { resumed.Inc() })

	go func() {
		defer func() { req1 <- struct{}{} }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://target?req=1", nil))
	}()

	<-req1 // Wait for req1 to arrive.

	go func() {
		defer func() { req2 <- struct{}{} }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://target?req=2", nil))
	}()

	<-req2 // Wait for req2 to arrive

	<-req1 // Allow req1 to pass.
	<-req1 // Wait for req1 to finish.

	<-req2 // Allow req2 to pass.
	<-req2 // Wait for req2 to finish.

	if got, want := pollFor(paused, 1), int64(1); got != want {
		t.Errorf("Pause was called %d times, want %d times", got, want)
	}

	if got, want := pollFor(resumed, 1), int64(1); got != want {
		t.Errorf("Resume was called %d times, want %d times", got, want)
	}
}

func BenchmarkConcurrencyStateProxyHandler(b *testing.B) {
	logger, _ := pkglogging.NewLogger("", "error")
	baseHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	stats := network.NewRequestStats(time.Now())

	promStatReporter, err := NewPrometheusStatsReporter(
		"ns", "testksvc", "testksvc",
		"pod", reportingPeriod)
	if err != nil {
		b.Fatal("Failed to create stats reporter:", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://example.com", nil)
	req.Header.Set(network.OriginalHostHeader, wantHost)

	tests := []struct {
		label        string
		breaker      *Breaker
		reportPeriod time.Duration
	}{{
		label:        "breaker-10-no-reports",
		breaker:      NewBreaker(BreakerParams{QueueDepth: 10, MaxConcurrency: 10, InitialCapacity: 10}),
		reportPeriod: time.Hour,
	}, {
		label:        "breaker-infinite-no-reports",
		breaker:      nil,
		reportPeriod: time.Hour,
	}, {
		label:        "breaker-10-many-reports",
		breaker:      NewBreaker(BreakerParams{QueueDepth: 10, MaxConcurrency: 10, InitialCapacity: 10}),
		reportPeriod: time.Microsecond,
	}, {
		label:        "breaker-infinite-many-reports",
		breaker:      nil,
		reportPeriod: time.Microsecond,
	}}

	for _, tc := range tests {
		reportTicker := time.NewTicker(tc.reportPeriod)

		go func() {
			for now := range reportTicker.C {
				promStatReporter.Report(stats.Report(now))
			}
		}()

		h := ConcurrencyStateHandler(logger, ProxyHandler(tc.breaker, stats, true /*tracingEnabled*/, baseHandler), nil, nil)
		b.Run("sequential-"+tc.label, func(b *testing.B) {
			resp := httptest.NewRecorder()
			for j := 0; j < b.N; j++ {
				h(resp, req)
			}
		})
		b.Run("parallel-"+tc.label, func(b *testing.B) {
			b.RunParallel(func(pb *testing.PB) {
				resp := httptest.NewRecorder()
				for pb.Next() {
					h(resp, req)
				}
			})
		})

		reportTicker.Stop()
	}
}

func pollFor(val *atomic.Int64, want int64) int64 {
	var lastVal int64
	wait.PollImmediate(1*time.Millisecond, 1*time.Second, func() (bool, error) {
		lastVal = val.Load()
		return lastVal == want, nil
	})
	return lastVal
}
