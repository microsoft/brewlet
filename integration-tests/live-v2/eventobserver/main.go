package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/typeurl/v2"
	"google.golang.org/protobuf/types/known/structpb"
)

const topic = "/brewlet/runtime-image"

type runtimeEvent struct {
	Namespace string         `json:"namespace"`
	Topic     string         `json:"topic"`
	Event     map[string]any `json:"event"`
}

func decode(envelope *events.Envelope) (runtimeEvent, error) {
	if envelope == nil || envelope.Namespace != "k8s.io" || envelope.Topic != topic || envelope.Event == nil {
		return runtimeEvent{}, fmt.Errorf("unexpected runtime event envelope")
	}
	value, err := typeurl.UnmarshalAny(envelope.Event)
	if err != nil {
		return runtimeEvent{}, err
	}
	payload, ok := value.(*structpb.Struct)
	if !ok {
		return runtimeEvent{}, fmt.Errorf("unexpected runtime event protobuf type")
	}
	return runtimeEvent{Namespace: envelope.Namespace, Topic: envelope.Topic, Event: payload.AsMap()}, nil
}

func observe(ctx context.Context, address string, output io.Writer) error {
	c, err := client.New(address)
	if err != nil {
		return err
	}
	defer c.Close()
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	envelopes, failures := c.EventService().Subscribe(ctx, `namespace=="k8s.io",topic=="/brewlet/runtime-image"`)
	encoder := json.NewEncoder(output)
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				return nil
			}
			return ctx.Err()
		case err := <-failures:
			if ctx.Err() == context.Canceled {
				return nil
			}
			if err == nil {
				return fmt.Errorf("containerd event subscription closed")
			}
			return err
		case envelope, ok := <-envelopes:
			if !ok {
				if ctx.Err() == context.Canceled {
					return nil
				}
				return fmt.Errorf("containerd event stream closed")
			}
			value, err := decode(envelope)
			if err != nil {
				return err
			}
			if err := encoder.Encode(value); err != nil {
				return err
			}
		}
	}
}

func main() {
	address := flag.String("address", "/run/containerd/containerd.sock", "owned node's containerd socket")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	if err := observe(ctx, *address, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
