package compute

import (
	"context"
	"fmt"

	gcompute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/option"
)

// GCP is a Compute implementation backed by the Compute Engine API. It uses
// Application Default Credentials (see `gcloud auth application-default login`).
type GCP struct {
	client   *gcompute.InstancesClient
	project  string
	zone     string
	instance string
}

// GCPConfig identifies the target instance.
type GCPConfig struct {
	Project  string
	Zone     string
	Instance string
}

// NewGCP builds a GCP-backed Compute client using ADC.
func NewGCP(ctx context.Context, cfg GCPConfig, opts ...option.ClientOption) (*GCP, error) {
	client, err := gcompute.NewInstancesRESTClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating compute client: %w", err)
	}
	return &GCP{
		client:   client,
		project:  cfg.Project,
		zone:     cfg.Zone,
		instance: cfg.Instance,
	}, nil
}

// Close releases the underlying client.
func (g *GCP) Close() error { return g.client.Close() }

// Status returns the normalized instance state.
func (g *GCP) Status(ctx context.Context) (State, error) {
	inst, err := g.client.Get(ctx, &computepb.GetInstanceRequest{
		Project:  g.project,
		Zone:     g.zone,
		Instance: g.instance,
	})
	if err != nil {
		return Unknown, fmt.Errorf("getting instance %s: %w", g.instance, err)
	}
	return ParseState(inst.GetStatus()), nil
}

// Start boots a TERMINATED instance.
func (g *GCP) Start(ctx context.Context) error {
	op, err := g.client.Start(ctx, &computepb.StartInstanceRequest{
		Project:  g.project,
		Zone:     g.zone,
		Instance: g.instance,
	})
	if err != nil {
		return fmt.Errorf("starting instance: %w", err)
	}
	return waitOp(ctx, op)
}

// Resume resumes a SUSPENDED instance.
func (g *GCP) Resume(ctx context.Context) error {
	op, err := g.client.Resume(ctx, &computepb.ResumeInstanceRequest{
		Project:  g.project,
		Zone:     g.zone,
		Instance: g.instance,
	})
	if err != nil {
		return fmt.Errorf("resuming instance: %w", err)
	}
	return waitOp(ctx, op)
}

// Suspend suspends a RUNNING instance.
func (g *GCP) Suspend(ctx context.Context) error {
	op, err := g.client.Suspend(ctx, &computepb.SuspendInstanceRequest{
		Project:  g.project,
		Zone:     g.zone,
		Instance: g.instance,
	})
	if err != nil {
		return fmt.Errorf("suspending instance: %w", err)
	}
	return waitOp(ctx, op)
}

// waiter is the subset of *gcompute.Operation used here.
type waiter interface {
	Wait(ctx context.Context, opts ...gax.CallOption) error
}

func waitOp(ctx context.Context, op waiter) error {
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for compute operation: %w", err)
	}
	return nil
}

var _ Compute = (*GCP)(nil)
