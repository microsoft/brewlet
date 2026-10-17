package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/containerd/containerd/v2/core/events"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestDecodeActualProtobufEnvelopePreservesExactRuntimeIdentity(t *testing.T) {
	want := map[string]any{"reason": "RuntimeImageResolved", "containerID": "task",
		"id": "runtime", "inventoryUID": "epoch", "generation": "9007199254740993", "digest": "sha256:source"}
	payload, err := structpb.NewStruct(want)
	if err != nil {
		t.Fatal(err)
	}
	packed, err := anypb.New(payload)
	if err != nil {
		t.Fatal(err)
	}
	value, err := decode(&events.Envelope{Namespace: "k8s.io", Topic: topic, Event: packed})
	if err != nil || !reflect.DeepEqual(value.Event, want) {
		t.Fatalf("wire event lost its runtime identity: %+v, %v", value, err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip runtimeEvent
	if err := json.Unmarshal(data, &roundtrip); err != nil || !reflect.DeepEqual(roundtrip, value) {
		t.Fatal("event output changed envelope or generation", string(data), err)
	}
}

func TestDecodeRejectsWrongNamespaceTopicOrPayload(t *testing.T) {
	empty, err := anypb.New(&emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	for _, envelope := range []*events.Envelope{
		nil,
		{Namespace: "foreign", Topic: topic, Event: empty},
		{Namespace: "k8s.io", Topic: "/tasks/start", Event: empty},
		{Namespace: "k8s.io", Topic: topic},
		{Namespace: "k8s.io", Topic: topic, Event: empty},
		{Namespace: "k8s.io", Topic: topic, Event: &anypb.Any{TypeUrl: "unknown.invalid.Event"}},
	} {
		if _, err := decode(envelope); err == nil {
			t.Fatalf("invalid event accepted: %+v", envelope)
		}
	}
}
