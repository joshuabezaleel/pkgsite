// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging"
	"golang.org/x/discovery/internal/log"
)

// Logger is the interface used to write request logs to GCP.
type Logger interface {
	Log(logging.Entry)
}

// LocalLogger is a logger that can be used when running locally (i.e.: not on
// GCP)
type LocalLogger struct{}

// Log implements the Logger interface via our internal log package.
func (l LocalLogger) Log(entry logging.Entry) {
	var msg strings.Builder
	if entry.HTTPRequest != nil {
		msg.WriteString(strconv.Itoa(entry.HTTPRequest.Status) + " ")
		if entry.HTTPRequest.Request != nil {
			msg.WriteString(entry.HTTPRequest.Request.URL.Path + " ")
		}
	}
	msg.WriteString(fmt.Sprint(entry.Payload))
	log.Info(context.Background(), msg.String())
}

// RequestLog returns a middleware that logs each incoming requests using the
// given logger. This logger replaces the built-in appengine request logger,
// which logged PII when behind IAP, in such a way that was impossible to turn
// off.
//
// Logs may be viewed in Pantheon by selecting the log source corresponding to
// the AppEngine service name (e.g. 'dev-worker').
func RequestLog(lg Logger) Middleware {
	return func(h http.Handler) http.Handler {
		return &handler{delegate: h, logger: lg}
	}
}

type handler struct {
	delegate http.Handler
	logger   Logger
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	traceID := r.Header.Get("X-Cloud-Trace-Context")
	h.logger.Log(logging.Entry{
		HTTPRequest: &logging.HTTPRequest{Request: r},
		Payload:     "request start",
		Severity:    logging.Info,
		Trace:       traceID,
	})
	w2 := &responseWriter{ResponseWriter: w}
	h.delegate.ServeHTTP(w2, r.WithContext(log.NewContextWithTraceID(r.Context(), traceID)))
	h.logger.Log(logging.Entry{
		HTTPRequest: &logging.HTTPRequest{
			Request: r,
			Status:  translateStatus(w2.status),
			Latency: time.Since(start),
		},
		Payload:  "request end",
		Severity: logging.Info,
		Trace:    traceID,
	})
}

type responseWriter struct {
	http.ResponseWriter

	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func translateStatus(code int) int {
	if code == 0 {
		return http.StatusOK
	}
	return code
}
