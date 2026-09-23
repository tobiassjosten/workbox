package compute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	gcompute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// lastActiveNamespace and lastActiveKey name the guest attribute the on-VM
// emitter writes the last-active unix timestamp to; a queryPath lookup returns
// matching entries under queryValue.items keyed by lastActiveKey.
const (
	lastActiveNamespace = "workbox"
	lastActiveKey       = "last_active"
)

// LastActiveQueryPath is that attribute's guest-attributes path. It is the
// contract with the activity emitter (infra/cloud-init.sh.tftpl) and the
// reconciler (infra/reconcile.yaml.tftpl).
const LastActiveQueryPath = lastActiveNamespace + "/" + lastActiveKey

// maxRenderedValue bounds how much of an untrusted guest-attribute value is
// quoted into an error.
const maxRenderedValue = 200

// ErrInvalidActivity marks a last-active value that is not a usable unix
// timestamp — non-numeric, zero or negative, so written by something other than
// the emitter. Callers treat such a value as unknown; it never counts as recent
// activity to the reconciler either (no activity for non-numeric or zero,
// long-stale activity for a negative), so it cannot hold off idle shutdown.
var ErrInvalidActivity = errors.New("last-active value is not a usable unix timestamp")

// GCP is a Compute implementation backed by the Compute Engine API. It uses
// Application Default Credentials (see `gcloud auth application-default login`).
type GCP struct {
	client   *gcompute.InstancesClient
	project  string
	zone     string
	instance string

	// mu guards the snapshot below.
	mu sync.Mutex
	// snapshot is the last instances.get result, kept so LastStart can reuse
	// the instance Status just fetched: `workbox status` reads both from one
	// round trip, and from one consistent view. Status itself never reads it —
	// WaitStable polls Status and must always see live state.
	snapshot   *computepb.Instance
	snapshotAt time.Time
}

// snapshotTTL bounds how stale a reused snapshot may be; long enough to cover
// one command, short enough that a later operation in the same process never
// reads a stale one.
const snapshotTTL = 5 * time.Second

// fetchInstance always calls the API and records the result.
func (g *GCP) fetchInstance(ctx context.Context) (*computepb.Instance, error) {
	inst, err := g.client.Get(ctx, &computepb.GetInstanceRequest{
		Project:  g.project,
		Zone:     g.zone,
		Instance: g.instance,
	})
	if err != nil {
		return nil, fmt.Errorf("getting instance %s: %w", g.instance, err)
	}
	g.mu.Lock()
	g.snapshot, g.snapshotAt = inst, time.Now()
	g.mu.Unlock()
	return inst, nil
}

// recentInstance returns the recorded instance when it is still fresh.
func (g *GCP) recentInstance() *computepb.Instance {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.snapshot != nil && time.Since(g.snapshotAt) < snapshotTTL {
		return g.snapshot
	}
	return nil
}

// invalidate drops the snapshot, so the next read goes to the API.
func (g *GCP) invalidate() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.snapshot = nil
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
	inst, err := g.fetchInstance(ctx)
	if err != nil {
		return Unknown, err
	}
	return ParseState(inst.GetStatus()), nil
}

// Start boots a TERMINATED instance.
func (g *GCP) Start(ctx context.Context) error {
	defer g.invalidate()
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
	defer g.invalidate()
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
	defer g.invalidate()
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

// LastActive reads the on-VM activity timestamp from guest attributes. ok is
// false when the attribute is absent (VM never reported, or asleep): a 404 is a
// normal "unknown", not an error. Any other failure is returned as an error.
func (g *GCP) LastActive(ctx context.Context) (time.Time, bool, error) {
	resp, err := g.client.GetGuestAttributes(ctx, &computepb.GetGuestAttributesInstanceRequest{
		Project:   g.project,
		Zone:      g.zone,
		Instance:  g.instance,
		QueryPath: ptr(LastActiveQueryPath),
	})
	if err != nil {
		if isNotFound(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("reading guest attribute %s: %w", LastActiveQueryPath, err)
	}
	return lastActiveFromResp(resp)
}

// LastStart reads the instance's lastStartTimestamp. ok is false when the
// field is empty (the instance has never started).
func (g *GCP) LastStart(ctx context.Context) (time.Time, bool, error) {
	inst := g.recentInstance()
	if inst == nil {
		var err error
		if inst, err = g.fetchInstance(ctx); err != nil {
			return time.Time{}, false, err
		}
	}
	return parseLastStart(inst.GetLastStartTimestamp())
}

// parseLastStart parses Compute's RFC 3339 lastStartTimestamp; an empty value
// means the instance has never started.
func parseLastStart(raw string) (time.Time, bool, error) {
	if raw == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parsing lastStartTimestamp %q: %w", raw, err)
	}
	return t, true, nil
}

// lastActiveFromResp extracts the last-active timestamp from a getGuestAttributes
// response. A queryPath lookup returns the value under queryValue.items (not
// variableValue) — the same field the reconciler reads. The query path names the
// exact key, so there is one entry; both this reader and the reconciler match it
// by key rather than by position. ok is false when the attribute is absent.
func lastActiveFromResp(resp *computepb.GuestAttributes) (time.Time, bool, error) {
	var raw string
	for _, it := range resp.GetQueryValue().GetItems() {
		if it.GetKey() == lastActiveKey {
			raw = it.GetValue()
			break
		}
	}
	if raw == "" {
		return time.Time{}, false, nil
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	// The reconciler reads 0 as unknown and would read a negative as activity
	// long past; reject both here rather than reporting activity in (or before)
	// 1970.
	if err != nil || secs <= 0 {
		// The VM writes this value, so bound what we render (as probeErr does
		// for ssh stderr) before it reaches a terminal or --json.
		shown := raw
		if r := []rune(shown); len(r) > maxRenderedValue {
			shown = string(r[:maxRenderedValue]) + "…"
		}
		return time.Time{}, false, fmt.Errorf("guest attribute %s=%q: %w", LastActiveQueryPath, shown, ErrInvalidActivity)
	}
	return time.Unix(secs, 0), true, nil
}

// isNotFound reports whether err is a 404 from the Compute API — for a guest
// attribute that means "never reported", not a failure.
func isNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}

// IsPermissionDenied reports whether err is a 403 from the Compute API, so
// callers can point at IAM without depending on the GCP error type.
func IsPermissionDenied(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusForbidden
}

// ptr returns a pointer to s, for optional proto string fields.
func ptr(s string) *string { return &s }

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

var (
	_ Compute  = (*GCP)(nil)
	_ Activity = (*GCP)(nil)
)
