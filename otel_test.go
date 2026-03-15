package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/juggernaut/webhook-sentry/certutil"
	"github.com/juggernaut/webhook-sentry/proxy"
	"github.com/juggernaut/webhook-sentry/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const otelProxyAddress = "127.0.0.1:13090"

// otelTestFixture extends the proxy test infrastructure with OTel
// in-memory exporters for asserting on spans and metrics.
type otelTestFixture struct {
	spanExporter   *tracetest.InMemoryExporter
	metricReader   *sdkmetric.ManualReader
	otelShutdown   func(context.Context) error
	proxyServer    *http.Server
	proxyAddress   string
	targetServers  []*http.Server
}

func (f *otelTestFixture) setUp(t *testing.T, configSetup func(*proxy.ProxyConfig), resourceAttrs ...attribute.KeyValue) *http.Client {
	t.Helper()

	if f.proxyAddress == "" {
		f.proxyAddress = otelProxyAddress
	}

	f.spanExporter = tracetest.NewInMemoryExporter()
	f.metricReader = sdkmetric.NewManualReader()

	ctx := context.Background()
	shutdown, err := telemetry.Initialize(ctx, telemetry.Config{
		ServiceName:        "test-webhook-sentry",
		ServiceVersion:     "test",
		SpanExporter:       f.spanExporter,
		MetricReader:       f.metricReader,
		ResourceAttributes: resourceAttrs,
	})
	if err != nil {
		t.Fatalf("Failed to initialize OTel: %s", err)
	}
	f.otelShutdown = shutdown

	proxyConfig := proxy.NewDefaultConfig()
	proxyConfig.InsecureSkipCidrDenyList = true
	if configSetup != nil {
		configSetup(proxyConfig)
	}

	proxy.SetupLogging(proxyConfig)
	proxy.SetupMetrics()

	proxyConfig.Listeners = []proxy.ListenerConfig{{
		Address: f.proxyAddress,
		Type:    proxy.HTTP,
	}}

	proxyServers := proxy.CreateProxyServers(proxyConfig)
	f.proxyServer = proxyServers[0]

	go func() {
		listener, err := net.Listen("tcp4", f.proxyAddress)
		if err != nil {
			t.Errorf("Could not start proxy listener: %s", err)
			return
		}
		f.proxyServer.Serve(listener)
	}()

	waitForOtelStartup(t, f.proxyAddress)

	tr := &http.Transport{
		Proxy: func(r *http.Request) (*url.URL, error) {
			return url.Parse("http://" + f.proxyAddress)
		},
	}
	return &http.Client{Transport: tr}
}

func (f *otelTestFixture) tearDown(t *testing.T) {
	t.Helper()
	if f.proxyServer != nil {
		f.proxyServer.Shutdown(context.Background())
	}
	for _, s := range f.targetServers {
		s.Shutdown(context.Background())
	}
	if f.otelShutdown != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f.otelShutdown(ctx)
	}
}

func (f *otelTestFixture) spans() tracetest.SpanStubs {
	return f.spanExporter.GetSpans()
}

func (f *otelTestFixture) collectMetrics(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := f.metricReader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Failed to collect metrics: %s", err)
	}
	return rm
}

func waitForOtelStartup(t *testing.T, address string) {
	t.Helper()
	for i := 0; i < 10; i++ {
		conn, err := net.Dial("tcp4", address)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Proxy did not start up on %s in time", address)
}

// startOtelTargetServer starts a target HTTP server that captures inbound
// headers for assertion (e.g. to verify traceparent propagation).
func startOtelTargetServer(t *testing.T, port string, headerChan chan http.Header) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) {
		if headerChan != nil {
			headerChan <- r.Header.Clone()
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Hello from OTel target")
	})
	mux.HandleFunc("/error", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "Internal Server Error")
	})

	server := &http.Server{
		Addr:    "127.0.0.1:" + port,
		Handler: mux,
	}
	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			t.Errorf("Target server failed: %s", err)
		}
	}()
	waitForOtelStartup(t, "127.0.0.1:"+port)
	return server
}

// findSpanByName returns the first span stub matching the given name.
func findSpanByName(spans tracetest.SpanStubs, name string) *tracetest.SpanStub {
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
	}
	return nil
}

// findSpansByName returns all span stubs matching the given name.
func findSpansByName(spans tracetest.SpanStubs, name string) []tracetest.SpanStub {
	var matched []tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == name {
			matched = append(matched, spans[i])
		}
	}
	return matched
}

// getAttr retrieves a string attribute value from a span stub.
func getAttr(span *tracetest.SpanStub, key string) string {
	for _, a := range span.Attributes {
		if string(a.Key) == key {
			return a.Value.Emit()
		}
	}
	return ""
}

// getIntAttr retrieves an integer attribute value from a span stub.
func getIntAttr(span *tracetest.SpanStub, key string) (int64, bool) {
	for _, a := range span.Attributes {
		if string(a.Key) == key {
			return a.Value.AsInt64(), true
		}
	}
	return 0, false
}

// hasAttr checks if a span has a specific attribute key.
func hasAttr(span *tracetest.SpanStub, key string) bool {
	for _, a := range span.Attributes {
		if string(a.Key) == key {
			return true
		}
	}
	return false
}

func TestOTelServerSpanCreated(t *testing.T) {
	fixture := &otelTestFixture{}
	headerChan := make(chan http.Header, 1)
	targetPort := "13180"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, headerChan)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/target", targetPort))
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("Expected 200, got %d", resp.StatusCode)
	}

	// Drain header chan
	<-headerChan

	spans := fixture.spans()
	serverSpan := findSpanByName(spans, "GET proxy")
	if serverSpan == nil {
		t.Fatalf("Expected to find a 'GET proxy' server span, found spans: %v", spanNames(spans))
	}

	if serverSpan.SpanKind != trace.SpanKindServer {
		t.Errorf("Expected SpanKindServer, got %s", serverSpan.SpanKind)
	}

	if v := getAttr(serverSpan, "http.request.method"); v != "GET" {
		t.Errorf("Expected http.request.method=GET, got %q", v)
	}
	if v := getAttr(serverSpan, "url.scheme"); v != "http" {
		t.Errorf("Expected url.scheme=http, got %q", v)
	}
	if !hasAttr(serverSpan, "network.peer.address") {
		t.Error("Expected network.peer.address attribute to be present")
	}
}

func TestOTelClientSpanCreated(t *testing.T) {
	fixture := &otelTestFixture{}
	targetPort := "13181"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, nil)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/target", targetPort))
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	spans := fixture.spans()
	// otelhttp.NewTransport creates CLIENT spans named "{METHOD} {host}"
	clientSpan := findSpanByName(spans, "GET localhost:"+targetPort)
	if clientSpan == nil {
		t.Fatalf("Expected to find a client span 'GET localhost:%s', found spans: %v", targetPort, spanNames(spans))
	}

	if clientSpan.SpanKind != trace.SpanKindClient {
		t.Errorf("Expected SpanKindClient, got %s", clientSpan.SpanKind)
	}
}

func TestOTelParentChildRelationship(t *testing.T) {
	fixture := &otelTestFixture{}
	targetPort := "13182"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, nil)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/target", targetPort))
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	spans := fixture.spans()
	serverSpan := findSpanByName(spans, "GET proxy")
	clientSpan := findSpanByName(spans, "GET localhost:"+targetPort)

	if serverSpan == nil || clientSpan == nil {
		t.Fatalf("Expected both server and client spans, found: %v", spanNames(spans))
	}

	// The client span's parent should be the server span
	if clientSpan.Parent.SpanID() != serverSpan.SpanContext.SpanID() {
		t.Errorf("Client span parent (%s) does not match server span ID (%s)",
			clientSpan.Parent.SpanID(), serverSpan.SpanContext.SpanID())
	}

	// They should share the same trace ID
	if clientSpan.SpanContext.TraceID() != serverSpan.SpanContext.TraceID() {
		t.Errorf("Client and server spans have different trace IDs: %s vs %s",
			clientSpan.SpanContext.TraceID(), serverSpan.SpanContext.TraceID())
	}
}

func TestOTelTraceContextExtraction(t *testing.T) {
	fixture := &otelTestFixture{}
	targetPort := "13183"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, nil)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	// Send a request with an explicit traceparent header
	traceID := "0af7651916cd43dd8448eb211c80319c"
	parentSpanID := "b7ad6b7169203331"
	traceparent := fmt.Sprintf("00-%s-%s-01", traceID, parentSpanID)

	req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%s/target", targetPort), nil)
	if err != nil {
		t.Fatalf("Failed to create request: %s", err)
	}
	req.Header.Set("Traceparent", traceparent)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	spans := fixture.spans()
	serverSpan := findSpanByName(spans, "GET proxy")
	if serverSpan == nil {
		t.Fatalf("Expected to find server span, found: %v", spanNames(spans))
	}

	// The server span should belong to the inbound trace
	if serverSpan.SpanContext.TraceID().String() != traceID {
		t.Errorf("Expected trace ID %s, got %s", traceID, serverSpan.SpanContext.TraceID())
	}

	// The server span's parent should be the inbound parent span ID
	if serverSpan.Parent.SpanID().String() != parentSpanID {
		t.Errorf("Expected parent span ID %s, got %s", parentSpanID, serverSpan.Parent.SpanID())
	}
}

func TestOTelTraceContextPropagatedToTarget(t *testing.T) {
	fixture := &otelTestFixture{}
	headerChan := make(chan http.Header, 1)
	targetPort := "13184"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, headerChan)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/target", targetPort))
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	headers := <-headerChan

	// The target should have received a traceparent header injected by otelhttp transport
	traceparent := headers.Get("Traceparent")
	if traceparent == "" {
		t.Fatal("Expected traceparent header to be propagated to target, but it was empty")
	}

	// Verify traceparent format: 00-{traceID}-{spanID}-{flags}
	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 {
		t.Fatalf("Expected traceparent to have 4 parts (00-traceID-spanID-flags), got: %s", traceparent)
	}
	if parts[0] != "00" {
		t.Errorf("Expected traceparent version 00, got %s", parts[0])
	}
}

func TestOTelNewTraceWithoutTraceparent(t *testing.T) {
	fixture := &otelTestFixture{}
	targetPort := "13185"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, nil)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	// Send request WITHOUT traceparent header — the proxy should generate a new trace
	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/target", targetPort))
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	spans := fixture.spans()
	serverSpan := findSpanByName(spans, "GET proxy")
	if serverSpan == nil {
		t.Fatalf("Expected server span, found: %v", spanNames(spans))
	}

	// The server span should have a valid trace ID (not zeroes)
	if !serverSpan.SpanContext.TraceID().IsValid() {
		t.Error("Expected a valid trace ID when no traceparent is provided")
	}

	// The server span should have no remote parent
	if serverSpan.Parent.IsValid() && serverSpan.Parent.IsRemote() {
		t.Error("Expected no remote parent when no traceparent header provided")
	}
}

func TestOTelInboundTraceparentPropagatedToTarget(t *testing.T) {
	fixture := &otelTestFixture{}
	headerChan := make(chan http.Header, 1)
	targetPort := "13186"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, headerChan)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	// Send request with inbound traceparent
	traceID := "0af7651916cd43dd8448eb211c80319c"
	parentSpanID := "b7ad6b7169203331"
	traceparent := fmt.Sprintf("00-%s-%s-01", traceID, parentSpanID)

	req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%s/target", targetPort), nil)
	if err != nil {
		t.Fatalf("Failed to create request: %s", err)
	}
	req.Header.Set("Traceparent", traceparent)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	headers := <-headerChan
	outboundTraceparent := headers.Get("Traceparent")
	if outboundTraceparent == "" {
		t.Fatal("Expected traceparent to be propagated to target")
	}

	// The outbound traceparent should contain the SAME trace ID as the inbound one
	parts := strings.Split(outboundTraceparent, "-")
	if len(parts) != 4 {
		t.Fatalf("Invalid traceparent format: %s", outboundTraceparent)
	}
	if parts[1] != traceID {
		t.Errorf("Expected same trace ID %s in outbound traceparent, got %s", traceID, parts[1])
	}

	// The outbound span ID should differ from the inbound parent (it's a new child span)
	if parts[2] == parentSpanID {
		t.Error("Expected outbound span ID to differ from inbound parent span ID")
	}
}

func TestOTelErrorSpanBlockedIP(t *testing.T) {
	fixture := &otelTestFixture{}

	// Use default deny list (which blocks private IPs) by NOT setting InsecureSkipCidrDenyList
	client := fixture.setUp(t, func(config *proxy.ProxyConfig) {
		config.InsecureSkipCidrDenyList = false
	})
	defer fixture.tearDown(t)

	resp, err := client.Get("http://127.0.0.1:12345/target")
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 403 {
		t.Fatalf("Expected 403, got %d", resp.StatusCode)
	}

	spans := fixture.spans()
	serverSpan := findSpanByName(spans, "GET proxy")
	if serverSpan == nil {
		t.Fatalf("Expected server span, found: %v", spanNames(spans))
	}

	errorCode, ok := getIntAttr(serverSpan, "alsoasked.proxy.error_code")
	if !ok {
		t.Fatal("Expected alsoasked.proxy.error_code attribute on error span")
	}
	if errorCode != int64(proxy.BlockedIPAddress) {
		t.Errorf("Expected alsoasked.proxy.error_code=%d, got %d", proxy.BlockedIPAddress, errorCode)
	}

	// codes.Error = 2
	if serverSpan.Status.Code != codes.Error {
		t.Errorf("Expected error status on span, got %v", serverSpan.Status)
	}
}

func TestOTelErrorSpanTimeout(t *testing.T) {
	fixture := &otelTestFixture{}

	// Start a server that never responds within time
	go func() {
		listener, err := net.Listen("tcp4", "127.0.0.1:13187")
		if err != nil {
			return
		}
		defer listener.Close()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Hold connection open but never respond
			go func(c net.Conn) {
				time.Sleep(30 * time.Second)
				c.Close()
			}(conn)
		}
	}()

	client := fixture.setUp(t, func(config *proxy.ProxyConfig) {
		config.ConnectionLifetime = 2 * time.Second
	})
	defer fixture.tearDown(t)

	waitForOtelStartup(t, "127.0.0.1:13187")

	resp, err := client.Get("http://localhost:13187/target")
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 502 {
		t.Fatalf("Expected 502, got %d", resp.StatusCode)
	}

	spans := fixture.spans()
	serverSpan := findSpanByName(spans, "GET proxy")
	if serverSpan == nil {
		t.Fatalf("Expected server span, found: %v", spanNames(spans))
	}

	errorCode, ok := getIntAttr(serverSpan, "alsoasked.proxy.error_code")
	if !ok {
		t.Fatal("Expected alsoasked.proxy.error_code attribute on timeout span")
	}
	if errorCode != int64(proxy.RequestTimedOut) {
		t.Errorf("Expected alsoasked.proxy.error_code=%d (RequestTimedOut), got %d", proxy.RequestTimedOut, errorCode)
	}
}

func TestOTelErrorSpanResponseTooLarge(t *testing.T) {
	fixture := &otelTestFixture{}
	targetPort := "13188"

	// Start a target that returns a large response
	mux := http.NewServeMux()
	mux.HandleFunc("/large", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, strings.Repeat("x", 1000000))
	})
	largeServer := &http.Server{Addr: "127.0.0.1:" + targetPort, Handler: mux}
	go func() {
		if err := largeServer.ListenAndServe(); err != http.ErrServerClosed {
			t.Errorf("Large server failed: %s", err)
		}
	}()
	waitForOtelStartup(t, "127.0.0.1:"+targetPort)

	client := fixture.setUp(t, func(config *proxy.ProxyConfig) {
		config.MaxResponseBodySize = 100 // Very small limit
	})
	fixture.targetServers = []*http.Server{largeServer}
	defer fixture.tearDown(t)

	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/large", targetPort))
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 502 {
		t.Fatalf("Expected 502, got %d", resp.StatusCode)
	}

	spans := fixture.spans()
	serverSpan := findSpanByName(spans, "GET proxy")
	if serverSpan == nil {
		t.Fatalf("Expected server span, found: %v", spanNames(spans))
	}

	errorCode, ok := getIntAttr(serverSpan, "alsoasked.proxy.error_code")
	if !ok {
		t.Fatal("Expected alsoasked.proxy.error_code attribute on response-too-large span")
	}
	if errorCode != int64(proxy.ResponseTooLarge) {
		t.Errorf("Expected alsoasked.proxy.error_code=%d (ResponseTooLarge), got %d", proxy.ResponseTooLarge, errorCode)
	}
}

func TestOTelErrorSpanInvalidURI(t *testing.T) {
	fixture := &otelTestFixture{}

	fixture.setUp(t, nil)
	defer fixture.tearDown(t)

	// Send a request with a relative URI (not absolute) — this should fail
	req, err := http.NewRequest("GET", "http://"+otelProxyAddress+"/target", nil)
	if err != nil {
		t.Fatalf("Failed to create request: %s", err)
	}
	// We must send directly to the proxy address with a relative path
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	spans := fixture.spans()
	serverSpan := findSpanByName(spans, "GET proxy")
	if serverSpan == nil {
		t.Fatalf("Expected server span, found: %v", spanNames(spans))
	}

	errorCode, ok := getIntAttr(serverSpan, "alsoasked.proxy.error_code")
	if !ok {
		t.Fatal("Expected alsoasked.proxy.error_code attribute on invalid URI span")
	}
	if errorCode != int64(proxy.InvalidRequestURI) {
		t.Errorf("Expected alsoasked.proxy.error_code=%d (InvalidRequestURI), got %d", proxy.InvalidRequestURI, errorCode)
	}
}

func TestOTelResponseStatusCodeAttribute(t *testing.T) {
	fixture := &otelTestFixture{}
	targetPort := "13189"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, nil)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/target", targetPort))
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	spans := fixture.spans()
	serverSpan := findSpanByName(spans, "GET proxy")
	if serverSpan == nil {
		t.Fatalf("Expected server span, got: %v", spanNames(spans))
	}

	statusCode, ok := getIntAttr(serverSpan, "http.response.status_code")
	if !ok {
		t.Fatal("Expected http.response.status_code attribute")
	}
	if statusCode != 200 {
		t.Errorf("Expected http.response.status_code=200, got %d", statusCode)
	}
}

func TestOTelPostMethodSpan(t *testing.T) {
	fixture := &otelTestFixture{}
	targetPort := "13190"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, nil)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	resp, err := client.Post(
		fmt.Sprintf("http://localhost:%s/target", targetPort),
		"application/json",
		strings.NewReader(`{"test": true}`),
	)
	if err != nil {
		t.Fatalf("Proxy POST request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	spans := fixture.spans()
	serverSpan := findSpanByName(spans, "POST proxy")
	if serverSpan == nil {
		t.Fatalf("Expected 'POST proxy' server span, found: %v", spanNames(spans))
	}
	if v := getAttr(serverSpan, "http.request.method"); v != "POST" {
		t.Errorf("Expected http.request.method=POST, got %q", v)
	}

	clientSpan := findSpanByName(spans, "POST localhost:"+targetPort)
	if clientSpan == nil {
		t.Fatalf("Expected 'POST localhost:%s' client span, found: %v", targetPort, spanNames(spans))
	}
}

func TestOTelMetricsDurationHistogram(t *testing.T) {
	fixture := &otelTestFixture{}
	targetPort := "13191"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, nil)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/target", targetPort))
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	rm := fixture.collectMetrics(t)
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "proxy.request.duration" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("Expected proxy.request.duration metric to be recorded")
	}
}

func TestOTelMetricsConnectionCounter(t *testing.T) {
	fixture := &otelTestFixture{}
	targetPort := "13192"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, nil)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	// Make a request to trigger connection state callbacks
	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/target", targetPort))
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	rm := fixture.collectMetrics(t)
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "proxy.connections" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("Expected proxy.connections metric to be recorded")
	}
}

func TestOTelTraceparentNotStrippedFromOutbound(t *testing.T) {
	fixture := &otelTestFixture{}
	headerChan := make(chan http.Header, 1)
	targetPort := "13193"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, headerChan)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	// Send with explicit traceparent
	traceID := "0af7651916cd43dd8448eb211c80319c"
	parentSpanID := "b7ad6b7169203331"
	traceparent := fmt.Sprintf("00-%s-%s-01", traceID, parentSpanID)

	req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%s/target", targetPort), nil)
	if err != nil {
		t.Fatalf("Failed to create request: %s", err)
	}
	req.Header.Set("Traceparent", traceparent)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	headers := <-headerChan

	// The traceparent should NOT be stripped — it should be present in outbound headers
	outTraceparent := headers.Get("Traceparent")
	if outTraceparent == "" {
		t.Fatal("Traceparent header was stripped from outbound request — should be preserved")
	}
}

func TestOTelW3CTraceContextPropagator(t *testing.T) {
	// Verify the propagator is set to W3C TraceContext
	prop := otel.GetTextMapPropagator()
	if prop == nil {
		t.Fatal("Expected TextMapPropagator to be set")
	}

	fields := prop.Fields()
	hasTraceparent := false
	for _, f := range fields {
		if f == "traceparent" {
			hasTraceparent = true
		}
	}
	if !hasTraceparent {
		t.Errorf("Expected W3C TraceContext propagator with 'traceparent' field, got fields: %v", fields)
	}
}

func TestOTelResourceAttributes(t *testing.T) {
	fixture := &otelTestFixture{}
	targetPort := "13194"

	client := fixture.setUp(t, nil,
		attribute.String("cloud.provider", "aws"),
		attribute.String("cloud.region", "eu-west-2"),
	)
	target := startOtelTargetServer(t, targetPort, nil)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/target", targetPort))
	if err != nil {
		t.Fatalf("Proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	rm := fixture.collectMetrics(t)
	resource := rm.Resource

	var attrs []attribute.KeyValue
	for iter := resource.Iter(); iter.Next(); {
		attrs = append(attrs, iter.Attribute())
	}

	attrMap := make(map[string]string)
	for _, a := range attrs {
		attrMap[string(a.Key)] = a.Value.Emit()
	}

	if v, ok := attrMap["service.name"]; !ok || v != "test-webhook-sentry" {
		t.Errorf("Expected service.name=test-webhook-sentry, got %q", v)
	}
	if v, ok := attrMap["service.version"]; !ok || v != "test" {
		t.Errorf("Expected service.version=test, got %q", v)
	}
	if v, ok := attrMap["cloud.provider"]; !ok || v != "aws" {
		t.Errorf("Expected cloud.provider=aws, got %q", v)
	}
	if v, ok := attrMap["cloud.region"]; !ok || v != "eu-west-2" {
		t.Errorf("Expected cloud.region=eu-west-2, got %q", v)
	}
}

func TestOTelMultipleRequestsCreateSeparateTraces(t *testing.T) {
	fixture := &otelTestFixture{}
	targetPort := "13195"

	client := fixture.setUp(t, nil)
	target := startOtelTargetServer(t, targetPort, nil)
	fixture.targetServers = []*http.Server{target}
	defer fixture.tearDown(t)

	// Make two requests
	for i := 0; i < 2; i++ {
		resp, err := client.Get(fmt.Sprintf("http://localhost:%s/target", targetPort))
		if err != nil {
			t.Fatalf("Request %d failed: %s", i, err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}

	spans := fixture.spans()
	serverSpans := findSpansByName(spans, "GET proxy")
	if len(serverSpans) != 2 {
		t.Fatalf("Expected 2 server spans for 2 requests, got %d. All spans: %v", len(serverSpans), spanNames(spans))
	}

	// The two server spans should have different trace IDs (separate traces)
	if serverSpans[0].SpanContext.TraceID() == serverSpans[1].SpanContext.TraceID() {
		t.Error("Expected different trace IDs for separate requests without traceparent")
	}
}

// spanNames returns a slice of span names for debugging.
func spanNames(spans tracetest.SpanStubs) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name
	}
	return names
}

const otelConnectProxyAddress = "127.0.0.1:13290"
const otelHTTPSTargetPort = "13196"

// startOtelHTTPSTargetServer starts an HTTPS target server using an in-memory TLS certificate.
func startOtelHTTPSTargetServer(t *testing.T, serverCert *tls.Certificate) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Hello from HTTPS target")
	})

	server := &http.Server{
		Addr:    "127.0.0.1:" + otelHTTPSTargetPort,
		Handler: mux,
	}
	go func() {
		config := &tls.Config{Certificates: []tls.Certificate{*serverCert}}
		listener, err := tls.Listen("tcp4", server.Addr, config)
		if err != nil {
			t.Errorf("Failed to listen on HTTPS target port: %s", err)
			return
		}
		if err := server.Serve(listener); err != http.ErrServerClosed {
			t.Errorf("HTTPS target server failed: %s", err)
		}
	}()
	waitForOtelStartup(t, "127.0.0.1:"+otelHTTPSTargetPort)
	return server
}

func TestOTelConnectSpan(t *testing.T) {
	certs := certutil.NewCertificateFixtures(t)

	fixture := &otelTestFixture{proxyAddress: otelConnectProxyAddress}
	fixture.setUp(t, func(config *proxy.ProxyConfig) {
		config.InsecureSkipCertVerification = true
		config.MitmIssuerCert = certs.RootCACert
	})

	httpsTarget := startOtelHTTPSTargetServer(t, certs.ServerCert)
	fixture.targetServers = []*http.Server{httpsTarget}
	defer fixture.tearDown(t)

	// Client trusts the MITM root CA and routes through the proxy.
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: func(r *http.Request) (*url.URL, error) {
				return url.Parse("http://" + otelConnectProxyAddress)
			},
			TLSClientConfig: &tls.Config{
				RootCAs: certs.RootCAs,
			},
		},
	}

	// An https:// request through an HTTP proxy triggers CONNECT tunneling.
	resp, err := client.Get(fmt.Sprintf("https://localhost:%s/target", otelHTTPSTargetPort))
	if err != nil {
		t.Fatalf("CONNECT proxy request failed: %s", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("Expected 200, got %d", resp.StatusCode)
	}

	// Close all idle connections so the proxy detects the tunnel close
	// and finishes the HandleHttpConnect goroutine (which ends the span).
	client.Transport.(*http.Transport).CloseIdleConnections()
	time.Sleep(500 * time.Millisecond)

	spans := fixture.spans()
	connectSpan := findSpanByName(spans, "CONNECT proxy")
	if connectSpan == nil {
		t.Fatalf("Expected to find a 'CONNECT proxy' span, found spans: %v", spanNames(spans))
	}

	if connectSpan.SpanKind != trace.SpanKindInternal {
		t.Errorf("Expected SpanKindInternal for CONNECT span, got %s", connectSpan.SpanKind)
	}

	if v := getAttr(connectSpan, "http.request.method"); v != "CONNECT" {
		t.Errorf("Expected http.request.method=CONNECT, got %q", v)
	}
	if !hasAttr(connectSpan, "network.peer.address") {
		t.Error("Expected network.peer.address attribute to be present on CONNECT span")
	}
	if !hasAttr(connectSpan, "url.full") {
		t.Error("Expected url.full attribute to be present on CONNECT span")
	}
}
