package pcopy

import (
	"crypto/rand"
	"net/http/httptest"
	"testing"
)

func TestSniffWriter_WriteHTML(t *testing.T) {
	rr := httptest.NewRecorder()
	sw := newSniffWriter(rr)
	sw.Write([]byte("<script>alert('hi')</script>"))
	assertStrEquals(t, "text/plain; charset=utf-8", rr.Header().Get("Content-Type"))
}

func TestSniffWriter_NoSniffWriterWriteHTML(t *testing.T) {
	// This test just makes sure that without the sniff-writer, we would get text/html

	rr := httptest.NewRecorder()
	rr.Write([]byte("<script>alert('hi')</script>"))
	assertStrEquals(t, "text/html; charset=utf-8", rr.Header().Get("Content-Type"))
}

func TestSniffWriter_WriteBinary(t *testing.T) {
	rr := httptest.NewRecorder()
	sw := newSniffWriter(rr)
	randomBytes := make([]byte, 199)
	rand.Read(randomBytes)
	sw.Write(randomBytes)
	assertStrEquals(t, "application/octet-stream", rr.Header().Get("Content-Type"))
}
