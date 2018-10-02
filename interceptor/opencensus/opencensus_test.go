// Copyright 2018, OpenCensus Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ocinterceptor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes/timestamp"
	"google.golang.org/grpc"

	"contrib.go.opencensus.io/exporter/ocagent"
	commonpb "github.com/census-instrumentation/opencensus-proto/gen-go/agent/common/v1"
	agenttracepb "github.com/census-instrumentation/opencensus-proto/gen-go/agent/trace/v1"
	tracepb "github.com/census-instrumentation/opencensus-proto/gen-go/trace/v1"
	"github.com/census-instrumentation/opencensus-service/interceptor/opencensus"
	"github.com/census-instrumentation/opencensus-service/spanreceiver"
	"go.opencensus.io/trace"
	"go.opencensus.io/trace/tracestate"
)

func TestOCInterceptor_endToEnd(t *testing.T) {
	sappender := newSpanAppender()

	_, port, doneFn := ocInterceptorOnGRPCServer(t, sappender, ocinterceptor.WithSpanBufferPeriod(100*time.Millisecond))
	defer doneFn()

	// Now the opencensus-agent exporter.
	oce, err := ocagent.NewExporter(ocagent.WithPort(uint16(port)), ocagent.WithInsecure())
	if err != nil {
		t.Fatalf("Failed to create the ocagent-exporter: %v", err)
	}

	trace.RegisterExporter(oce)

	defer func() {
		oce.Stop()
		trace.UnregisterExporter(oce)
	}()

	now := time.Now().UTC()
	clientSpanData := &trace.SpanData{
		StartTime: now.Add(-10 * time.Second),
		EndTime:   now.Add(20 * time.Second),
		SpanContext: trace.SpanContext{
			TraceID:      trace.TraceID{0x4F, 0x4E, 0x4D, 0x4C, 0x4B, 0x4A, 0x49, 0x48, 0x47, 0x46, 0x45, 0x44, 0x43, 0x42, 0x41},
			SpanID:       trace.SpanID{0x7F, 0x7E, 0x7D, 0x7C, 0x7B, 0x7A, 0x79, 0x78},
			TraceOptions: trace.TraceOptions(0x01),
		},
		ParentSpanID: trace.SpanID{0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37},
		Name:         "ClientSpan",
		Status:       trace.Status{Code: trace.StatusCodeInternal, Message: "Blocked by firewall"},
		SpanKind:     trace.SpanKindClient,
	}

	serverSpanData := &trace.SpanData{
		StartTime: now.Add(-5 * time.Second),
		EndTime:   now.Add(10 * time.Second),
		SpanContext: trace.SpanContext{
			TraceID:      trace.TraceID{0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E},
			SpanID:       trace.SpanID{0xF0, 0xF1, 0xF2, 0xF3, 0xF4, 0xF5, 0xF6, 0xF7},
			TraceOptions: trace.TraceOptions(0x01),
			Tracestate:   &tracestate.Tracestate{},
		},
		ParentSpanID: trace.SpanID{0x38, 0x39, 0x3A, 0x3B, 0x3C, 0x3D, 0x3E, 0x3F},
		Name:         "ServerSpan",
		Status:       trace.Status{Code: trace.StatusCodeOK, Message: "OK"},
		SpanKind:     trace.SpanKindServer,
		Links: []trace.Link{
			{
				TraceID: trace.TraceID{0x4F, 0x4E, 0x4D, 0x4C, 0x4B, 0x4A, 0x49, 0x48, 0x47, 0x46, 0x45, 0x44, 0x43, 0x42, 0x41, 0x40},
				SpanID:  trace.SpanID{0x7F, 0x7E, 0x7D, 0x7C, 0x7B, 0x7A, 0x79, 0x78},
				Type:    trace.LinkTypeParent,
			},
		},
	}

	oce.ExportSpan(serverSpanData)
	oce.ExportSpan(clientSpanData)
	// Give them some time to be exported.
	<-time.After(100 * time.Millisecond)

	oce.Flush()

	// Give them some time to be exported.
	<-time.After(150 * time.Millisecond)

	// Now span inspection and verification time!
	var gotSpans []*tracepb.Span
	sappender.forEachEntry(func(_ *commonpb.Node, spans []*tracepb.Span) {
		gotSpans = append(gotSpans, spans...)
	})

	wantSpans := []*tracepb.Span{
		{
			TraceId:      serverSpanData.TraceID[:],
			SpanId:       serverSpanData.SpanID[:],
			ParentSpanId: serverSpanData.ParentSpanID[:],
			Name:         &tracepb.TruncatableString{Value: "ServerSpan"},
			Kind:         tracepb.Span_SERVER,
			StartTime:    timeToTimestamp(serverSpanData.StartTime),
			EndTime:      timeToTimestamp(serverSpanData.EndTime),
			Status:       &tracepb.Status{Code: int32(serverSpanData.Status.Code), Message: serverSpanData.Status.Message},
			Tracestate:   &tracepb.Span_Tracestate{},
			Links: &tracepb.Span_Links{
				Link: []*tracepb.Span_Link{
					{
						TraceId: []byte{0x4F, 0x4E, 0x4D, 0x4C, 0x4B, 0x4A, 0x49, 0x48, 0x47, 0x46, 0x45, 0x44, 0x43, 0x42, 0x41, 0x40},
						SpanId:  []byte{0x7F, 0x7E, 0x7D, 0x7C, 0x7B, 0x7A, 0x79, 0x78},
						Type:    tracepb.Span_Link_PARENT_LINKED_SPAN,
					},
				},
			},
		},
		{
			TraceId:      clientSpanData.TraceID[:],
			SpanId:       clientSpanData.SpanID[:],
			ParentSpanId: clientSpanData.ParentSpanID[:],
			Name:         &tracepb.TruncatableString{Value: "ClientSpan"},
			Kind:         tracepb.Span_CLIENT,
			StartTime:    timeToTimestamp(clientSpanData.StartTime),
			EndTime:      timeToTimestamp(clientSpanData.EndTime),
			Status:       &tracepb.Status{Code: int32(clientSpanData.Status.Code), Message: clientSpanData.Status.Message},
		},
	}

	if g, w := len(gotSpans), len(wantSpans); g != w {
		t.Errorf("SpanCount: got %d want %d", g, w)
	}

	if !reflect.DeepEqual(gotSpans, wantSpans) {
		gotBlob, _ := json.MarshalIndent(gotSpans, "", "  ")
		wantBlob, _ := json.MarshalIndent(wantSpans, "", "  ")
		t.Errorf("GotSpans:\n%s\nWantSpans:\n%s", gotBlob, wantBlob)
	}
}

// Issue #43. Export should support node multiplexing.
// The goal is to ensure that OCInterceptor can always support
// a passthrough mode where it initiates Export normally by firstly
// receiving the initiator node. However ti should still be able to
// accept nodes from downstream sources, but if a node isn't specified in
// an exportTrace request, assume it is from the last received and non-nil node.
func TestExportMultiplexing(t *testing.T) {
	spanSink := newSpanAppender()

	_, port, doneFn := ocInterceptorOnGRPCServer(t, spanSink, ocinterceptor.WithSpanBufferPeriod(90*time.Millisecond))
	defer doneFn()

	addr := fmt.Sprintf(":%d", port)
	cc, err := grpc.Dial(addr, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		t.Fatalf("Failed to create the gRPC client connection: %v", err)
	}
	defer cc.Close()

	svc := agenttracepb.NewTraceServiceClient(cc)
	traceClient, err := svc.Export(context.Background())
	if err != nil {
		t.Fatalf("Failed to create the traceClient: %v", err)
	}

	// Step 1) The intiation
	initiatingNode := &commonpb.Node{
		Identifier: &commonpb.ProcessIdentifier{
			Pid:      1,
			HostName: "multiplexer",
		},
		LibraryInfo: &commonpb.LibraryInfo{Language: commonpb.LibraryInfo_JAVA},
	}

	if err := traceClient.Send(&agenttracepb.ExportTraceServiceRequest{Node: initiatingNode}); err != nil {
		t.Fatalf("Failed to send the initiating message: %v", err)
	}

	// Step 1a) Send some spans without a node, they should be registered as coming from the initiating node.
	sLi := []*tracepb.Span{{TraceId: []byte("1234567890abcde")}}
	if err := traceClient.Send(&agenttracepb.ExportTraceServiceRequest{Node: nil, Spans: sLi}); err != nil {
		t.Fatalf("Failed to send the proxied message from app1: %v", err)
	}

	// Step 2) Send a "proxied" trace message from app1 with "node1"
	node1 := &commonpb.Node{
		Identifier:  &commonpb.ProcessIdentifier{Pid: 9489, HostName: "nodejs-host"},
		LibraryInfo: &commonpb.LibraryInfo{Language: commonpb.LibraryInfo_NODE_JS},
	}
	sL1 := []*tracepb.Span{{TraceId: []byte("abcdefghijklmno")}}
	if err := traceClient.Send(&agenttracepb.ExportTraceServiceRequest{Node: node1, Spans: sL1}); err != nil {
		t.Fatalf("Failed to send the proxied message from app1: %v", err)
	}

	// Step 3) Send a trace message without a node but with spans: this
	// should be registered as belonging to the last used node i.e. "node1".
	sLn1 := []*tracepb.Span{{TraceId: []byte("ABCDEFGHIJKLMNO")}, {TraceId: []byte("1234567890abcde")}}
	if err := traceClient.Send(&agenttracepb.ExportTraceServiceRequest{Node: nil, Spans: sLn1}); err != nil {
		t.Fatalf("Failed to send the proxied message without a node: %v", err)
	}

	// Step 4) Send a trace message from a differently proxied node "node2" from app2
	node2 := &commonpb.Node{
		Identifier:  &commonpb.ProcessIdentifier{Pid: 7752, HostName: "golang-host"},
		LibraryInfo: &commonpb.LibraryInfo{Language: commonpb.LibraryInfo_GO_LANG},
	}
	sL2 := []*tracepb.Span{{TraceId: []byte("_B_D_F_H_J_L_N_")}}
	if err := traceClient.Send(&agenttracepb.ExportTraceServiceRequest{Node: node2, Spans: sL2}); err != nil {
		t.Fatalf("Failed to send the proxied message from app2: %v", err)
	}

	// Step 5a) Send a trace message without a node but with spans: this
	// should be registered as belonging to the last used node i.e. "node2".
	sLn2a := []*tracepb.Span{{TraceId: []byte("_BCDEFGHIJKLMN_")}, {TraceId: []byte("_234567890abcd_")}}
	if err := traceClient.Send(&agenttracepb.ExportTraceServiceRequest{Node: nil, Spans: sLn2a}); err != nil {
		t.Fatalf("Failed to send the proxied message without a node: %v", err)
	}

	// Step 5b)
	sLn2b := []*tracepb.Span{{TraceId: []byte("_xxxxxxxxxxxxx_")}, {TraceId: []byte("B234567890abcdA")}}
	if err := traceClient.Send(&agenttracepb.ExportTraceServiceRequest{Node: nil, Spans: sLn2b}); err != nil {
		t.Fatalf("Failed to send the proxied message without a node: %v", err)
	}
	// Give the process sometime to send data over the wire and perform batching
	<-time.After(150 * time.Millisecond)

	// Examination time!
	resultsMapping := make(map[string][]*tracepb.Span)

	spanSink.forEachEntry(func(node *commonpb.Node, spans []*tracepb.Span) {
		resultsMapping[nodeToKey(node)] = spans
	})

	// First things first, we expect exactly 3 unique keys
	// 1. Initiating Node
	// 2. Node 1
	// 3. Node 2
	if g, w := len(resultsMapping), 3; g != w {
		t.Errorf("Got %d keys in the results map; Wanted exactly %d\n\nResultsMapping: %+v\n", g, w, resultsMapping)
	}

	// Want span counts
	wantSpanCounts := map[string]int{
		nodeToKey(initiatingNode): 1,
		nodeToKey(node1):          3,
		nodeToKey(node2):          5,
	}
	for key, wantSpanCounts := range wantSpanCounts {
		gotSpanCounts := len(resultsMapping[key])
		if gotSpanCounts != wantSpanCounts {
			t.Errorf("Key=%q gotSpanCounts %d wantSpanCounts %d", key, gotSpanCounts, wantSpanCounts)
		}
	}

	// Now ensure that the exported spans match up exactly with
	// the nodes and the last seen node expectation/behavior.
	// (or at least their serialized equivalents match up)
	wantContents := map[string][]*tracepb.Span{
		nodeToKey(initiatingNode): sLi,
		nodeToKey(node1):          append(sL1, sLn1...),
		nodeToKey(node2):          append(sL2, append(sLn2a, sLn2b...)...),
	}

	gotBlob, _ := json.Marshal(resultsMapping)
	wantBlob, _ := json.Marshal(wantContents)
	if !bytes.Equal(gotBlob, wantBlob) {
		t.Errorf("Unequal serialization results\nGot:\n\t%s\nWant:\n\t%s\n", gotBlob, wantBlob)
	}
}

func nodeToKey(n *commonpb.Node) string {
	blob, _ := proto.Marshal(n)
	return string(blob)
}

type spanAppender struct {
	sync.RWMutex
	spansPerNode map[*commonpb.Node][]*tracepb.Span
}

func newSpanAppender() *spanAppender {
	return &spanAppender{spansPerNode: make(map[*commonpb.Node][]*tracepb.Span)}
}

var _ spanreceiver.SpanReceiver = (*spanAppender)(nil)

func (sa *spanAppender) ReceiveSpans(node *commonpb.Node, spans ...*tracepb.Span) (*spanreceiver.Acknowledgement, error) {
	sa.Lock()
	defer sa.Unlock()

	sa.spansPerNode[node] = append(sa.spansPerNode[node], spans...)

	return &spanreceiver.Acknowledgement{SavedSpans: uint64(len(spans))}, nil
}

func ocInterceptorOnGRPCServer(t *testing.T, sr spanreceiver.SpanReceiver, opts ...ocinterceptor.OCOption) (oci *ocinterceptor.OCInterceptor, port int, done func()) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("Failed to find an available address to run the gRPC server: %v", err)
	}

	doneFnList := []func(){func() { ln.Close() }}
	done = func() {
		for _, doneFn := range doneFnList {
			doneFn()
		}
	}

	_, port, err = hostPortFromAddr(ln.Addr())
	if err != nil {
		done()
		t.Fatalf("Failed to parse host:port from listener address: %s error: %v", ln.Addr(), err)
	}

	if err != nil {
		done()
		t.Fatalf("Failed to create new agent: %v", err)
	}

	oci, err = ocinterceptor.New(sr, opts...)
	if err != nil {
		t.Fatalf("Failed to create the OCInterceptor: %v", err)
	}

	// Now run it as a gRPC server
	srv := grpc.NewServer()
	agenttracepb.RegisterTraceServiceServer(srv, oci)
	go func() {
		_ = srv.Serve(ln)
	}()

	return oci, port, done
}

func hostPortFromAddr(addr net.Addr) (host string, port int, err error) {
	addrStr := addr.String()
	sepIndex := strings.LastIndex(addrStr, ":")
	if sepIndex < 0 {
		return "", -1, errors.New("failed to parse host:port")
	}
	host, portStr := addrStr[:sepIndex], addrStr[sepIndex+1:]
	port, err = strconv.Atoi(portStr)
	return host, port, err
}

func (sa *spanAppender) forEachEntry(fn func(*commonpb.Node, []*tracepb.Span)) {
	sa.RLock()
	defer sa.RUnlock()

	for node, spans := range sa.spansPerNode {
		fn(node, spans)
	}
}

func timeToTimestamp(t time.Time) *timestamp.Timestamp {
	nanoTime := t.UnixNano()
	return &timestamp.Timestamp{
		Seconds: nanoTime / 1e9,
		Nanos:   int32(nanoTime % 1e9),
	}
}