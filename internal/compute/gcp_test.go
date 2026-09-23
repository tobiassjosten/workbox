package compute

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
)

func TestLastActiveFromResp(t *testing.T) {
	items := func(entries ...*computepb.GuestAttributesEntry) *computepb.GuestAttributes {
		return &computepb.GuestAttributes{QueryValue: &computepb.GuestAttributesValue{Items: entries}}
	}
	for _, tc := range []struct {
		name          string
		resp          *computepb.GuestAttributes
		wantTime      time.Time
		wantOK        bool
		wantErr       bool
		wantTruncated bool // the rendered value must be capped
	}{
		{
			// A queryPath lookup returns the value under queryValue.items — the
			// shape the live getGuestAttributes API returns for workbox/last_active.
			name:     "items response",
			resp:     items(&computepb.GuestAttributesEntry{Namespace: ptr("workbox"), Key: ptr(lastActiveKey), Value: ptr("1789930541")}),
			wantTime: time.Unix(1789930541, 0),
			wantOK:   true,
		},
		{
			name: "matching key after another entry",
			resp: items(
				&computepb.GuestAttributesEntry{Key: ptr("other"), Value: ptr("7")},
				&computepb.GuestAttributesEntry{Key: ptr(lastActiveKey), Value: ptr("42")},
			),
			wantTime: time.Unix(42, 0),
			wantOK:   true,
		},
		{
			name: "only a foreign key",
			resp: items(&computepb.GuestAttributesEntry{Key: ptr("other"), Value: ptr("7")}),
		},
		{
			name: "absent attribute",
			resp: &computepb.GuestAttributes{},
		},
		{
			// The rendered value is capped; the error still says it is invalid.
			name:          "oversized value",
			resp:          items(&computepb.GuestAttributesEntry{Key: ptr(lastActiveKey), Value: ptr(strings.Repeat("9", 500))}),
			wantErr:       true,
			wantTruncated: true,
		},
		{
			// The cap must not reach the parser: leading zeros are valid, so
			// truncating before ParseInt would turn this into 0 (invalid).
			name:     "long but valid value",
			resp:     items(&computepb.GuestAttributesEntry{Key: ptr(lastActiveKey), Value: ptr(strings.Repeat("0", 490) + "1789930541")}),
			wantTime: time.Unix(1789930541, 0),
			wantOK:   true,
		},
		{
			name:    "negative value",
			resp:    items(&computepb.GuestAttributesEntry{Key: ptr(lastActiveKey), Value: ptr("-1")}),
			wantErr: true,
		},
		{
			name:    "zero value (the reconciler's unknown)",
			resp:    items(&computepb.GuestAttributesEntry{Key: ptr(lastActiveKey), Value: ptr("0")}),
			wantErr: true,
		},
		{
			name:    "non-numeric value",
			resp:    items(&computepb.GuestAttributesEntry{Key: ptr(lastActiveKey), Value: ptr("not-a-number")}),
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := lastActiveFromResp(tc.resp)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidActivity) {
				t.Errorf("err = %v, want ErrInvalidActivity", err)
			}
			if tc.wantTruncated {
				if !strings.Contains(err.Error(), "…") {
					t.Errorf("err = %v, want the rendered value truncated", err)
				}
				if n := strings.Count(err.Error(), "9"); n > maxRenderedValue {
					t.Errorf("err renders %d value runes, want at most %d", n, maxRenderedValue)
				}
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK && !got.Equal(tc.wantTime) {
				t.Errorf("time = %v, want %v", got, tc.wantTime)
			}
		})
	}
}

// The snapshot LastStart reuses must expire and must be dropped by invalidate,
// or status would report a stale boot grace. (That Start/Resume/Suspend call
// invalidate is not pinned here — it needs a client behind an interface.)
func TestInstanceSnapshot(t *testing.T) {
	g := &GCP{}
	inst := &computepb.Instance{LastStartTimestamp: ptr("2026-06-15T20:00:00Z")}
	g.snapshot, g.snapshotAt = inst, time.Now()
	if got := g.recentInstance(); got != inst {
		t.Errorf("recentInstance = %v, want the fresh snapshot", got)
	}
	g.snapshotAt = time.Now().Add(-snapshotTTL)
	if got := g.recentInstance(); got != nil {
		t.Errorf("recentInstance = %v, want nil for an expired snapshot", got)
	}
	g.snapshot, g.snapshotAt = inst, time.Now()
	g.invalidate()
	if got := g.recentInstance(); got != nil {
		t.Errorf("recentInstance = %v, want nil after invalidate", got)
	}
}

func TestIsNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"404", &googleapi.Error{Code: http.StatusNotFound}, true},
		{"wrapped 404", fmt.Errorf("reading attribute: %w", &googleapi.Error{Code: http.StatusNotFound}), true},
		{"403", &googleapi.Error{Code: http.StatusForbidden}, false},
		{"plain error", errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNotFound(tc.err); got != tc.want {
				t.Errorf("isNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsPermissionDenied(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"403", &googleapi.Error{Code: http.StatusForbidden}, true},
		{"wrapped 403", fmt.Errorf("reading attribute: %w", &googleapi.Error{Code: http.StatusForbidden}), true},
		{"404", &googleapi.Error{Code: http.StatusNotFound}, false},
		{"plain error", errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPermissionDenied(tc.err); got != tc.want {
				t.Errorf("IsPermissionDenied(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The guest-attribute path is the contract with the activity emitter
// (infra/cloud-init.sh.tftpl) and the reconciler's queryPath, both rendered from
// locals.activity_key — change all of them together.
func TestLastActiveQueryPathIsTheWireValue(t *testing.T) {
	raw, err := os.ReadFile("../../infra/locals.tf")
	if err != nil {
		t.Fatal(err)
	}
	want := `activity_key = "` + LastActiveQueryPath + `"`
	if !strings.Contains(string(raw), want) {
		t.Errorf("infra/locals.tf does not contain %s", want)
	}
	// The bare key the reconciler matches entries by is derived from the same
	// local, not hard-coded.
	if want := `activity_key_name = split("/", local.activity_key)[1]`; !strings.Contains(string(raw), want) {
		t.Errorf("infra/locals.tf does not contain %s", want)
	}
	// Both templates must use the local rather than a hard-coded path.
	reconciler, err := os.ReadFile("../../infra/reconcile.yaml.tftpl")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`queryPath: "${activity_key}"`,
		`== "${activity_key_name}"`, // entries are matched by the derived key
	} {
		if !strings.Contains(string(reconciler), want) {
			t.Errorf("infra/reconcile.yaml.tftpl does not contain %s", want)
		}
	}
	cloudInit, err := os.ReadFile("../../infra/cloud-init.sh.tftpl")
	if err != nil {
		t.Fatal(err)
	}
	// Specifically inside the emitter the startup script installs — the
	// bootstrap report writes the same URL, so a file-wide match would pass
	// with a mangled emitter.
	_, emitter, ok := strings.Cut(string(cloudInit), "<<'ACTIVITY'")
	if !ok {
		t.Fatal("infra/cloud-init.sh.tftpl no longer has the ACTIVITY heredoc")
	}
	emitter, _, ok = strings.Cut(emitter, "\nACTIVITY\n")
	if !ok {
		t.Fatal("infra/cloud-init.sh.tftpl ACTIVITY heredoc is unterminated")
	}
	if want := `guest-attributes/${activity_key}`; !strings.Contains(emitter, want) {
		t.Errorf("the activity emitter does not write %s", want)
	}
}

func TestParseLastStart(t *testing.T) {
	got, ok, err := parseLastStart("2026-06-15T23:05:07.123-07:00")
	if err != nil || !ok {
		t.Fatalf("parseLastStart = %v, %v, %v", got, ok, err)
	}
	if want := time.Date(2026, 6, 16, 6, 5, 7, 123e6, time.UTC); !got.Equal(want) {
		t.Errorf("parseLastStart = %v, want %v", got, want)
	}
	if _, ok, err := parseLastStart(""); ok || err != nil {
		t.Errorf("empty: ok=%v err=%v, want not ok and no error", ok, err)
	}
	if _, _, err := parseLastStart("yesterday"); err == nil {
		t.Error("malformed: want an error")
	}
}
