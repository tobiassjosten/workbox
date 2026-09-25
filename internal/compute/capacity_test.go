package compute

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
)

// capacityOp builds the operation Compute Engine returns for an exhausted zone:
// a top-level error code plus error_details carrying the reason and metadata.
func capacityOp(code, zone, vmType, zonesAvailable string) *computepb.Operation {
	return &computepb.Operation{
		Zone: ptr("https://www.googleapis.com/compute/v1/projects/p/zones/" + zone),
		Error: &computepb.Error{Errors: []*computepb.Errors{{
			Code:    ptr(code),
			Message: ptr("The zone does not have enough resources available."),
			ErrorDetails: []*computepb.ErrorDetails{
				{ErrorInfo: &computepb.ErrorInfo{
					Domain: ptr("compute.googleapis.com"),
					Reason: ptr("resource_availability"),
					Metadatas: map[string]string{
						"attachment":     "",
						"vmType":         vmType,
						"zone":           zone,
						"zonesAvailable": zonesAvailable,
					},
				}},
				{LocalizedMessage: &computepb.LocalizedMessage{
					Locale:  ptr("en-US"),
					Message: ptr("A " + vmType + " VM instance is currently unavailable in the " + zone + " zone."),
				}},
			},
		}}},
	}
}

func TestCapacityErrorFromOp(t *testing.T) {
	cause := errors.New("googleapi: Error 503")

	tests := []struct {
		name           string
		op             *computepb.Operation
		wantNil        bool
		wantZone       string
		wantMachine    string
		wantZones      []string
		wantErrorLine  string
		wantMessagePfx string
	}{
		{
			name:           "exhausted with all details",
			op:             capacityOp(codePoolExhausted, "europe-north2-a", "e2-custom-4-8192", "europe-north2-b,europe-north2-c"),
			wantZone:       "europe-north2-a",
			wantMachine:    "e2-custom-4-8192",
			wantZones:      []string{"europe-north2-b", "europe-north2-c"},
			wantErrorLine:  "europe-north2-a has no capacity for e2-custom-4-8192 right now (ZONE_RESOURCE_POOL_EXHAUSTED)",
			wantMessagePfx: "A e2-custom-4-8192 VM instance",
		},
		{
			name:           "with-details variant and no alternative zones",
			op:             capacityOp(codePoolExhaustedWithDetails, "europe-north2-a", "e2-custom-4-8192", ""),
			wantZone:       "europe-north2-a",
			wantMachine:    "e2-custom-4-8192",
			wantErrorLine:  "europe-north2-a has no capacity for e2-custom-4-8192 right now (ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS)",
			wantMessagePfx: "A e2-custom-4-8192 VM instance",
		},
		{
			name: "no error details at all still classifies",
			op: &computepb.Operation{Error: &computepb.Error{Errors: []*computepb.Errors{{
				Code:    ptr(codePoolExhausted),
				Message: ptr("The zone does not have enough resources available."),
			}}}},
			wantErrorLine:  "the zone has no capacity for this machine type right now (ZONE_RESOURCE_POOL_EXHAUSTED)",
			wantMessagePfx: "The zone does not have",
		},
		{
			// Errors.location names the request field at fault ("zone"), not a
			// zone, so it must never be read as one.
			name: "the error location is not used as a zone",
			op: &computepb.Operation{
				Zone: ptr("https://www.googleapis.com/compute/v1/projects/p/zones/europe-north2-a"),
				Error: &computepb.Error{Errors: []*computepb.Errors{{
					Code:     ptr(codePoolExhausted),
					Location: ptr("zone"),
				}}},
			},
			wantZone:      "europe-north2-a",
			wantErrorLine: "europe-north2-a has no capacity for this machine type right now (ZONE_RESOURCE_POOL_EXHAUSTED)",
		},
		{
			name: "zone falls back to the operation zone URL",
			op: &computepb.Operation{
				Zone:  ptr("https://www.googleapis.com/compute/v1/projects/p/zones/europe-north2-a"),
				Error: &computepb.Error{Errors: []*computepb.Errors{{Code: ptr(codePoolExhausted)}}},
			},
			wantZone:      "europe-north2-a",
			wantErrorLine: "europe-north2-a has no capacity for this machine type right now (ZONE_RESOURCE_POOL_EXHAUSTED)",
		},
		{
			name: "metadata zone wins over the operation zone",
			op: &computepb.Operation{
				Zone: ptr("https://www.googleapis.com/compute/v1/projects/p/zones/europe-west3-b"),
				Error: &computepb.Error{Errors: []*computepb.Errors{{
					Code: ptr(codePoolExhausted),
					ErrorDetails: []*computepb.ErrorDetails{{ErrorInfo: &computepb.ErrorInfo{
						Metadatas: map[string]string{"zone": "europe-north2-a"},
					}}},
				}}},
			},
			wantZone:      "europe-north2-a",
			wantErrorLine: "europe-north2-a has no capacity for this machine type right now (ZONE_RESOURCE_POOL_EXHAUSTED)",
		},
		{
			// Sanitizing runs before the emptiness test, so a value that is
			// nothing but control characters falls through to the next candidate.
			name: "a metadata zone of only control characters falls through",
			op: &computepb.Operation{
				Zone: ptr("https://www.googleapis.com/compute/v1/projects/p/zones/europe-north2-a"),
				Error: &computepb.Error{Errors: []*computepb.Errors{{
					Code: ptr(codePoolExhausted),
					ErrorDetails: []*computepb.ErrorDetails{{ErrorInfo: &computepb.ErrorInfo{
						Metadatas: map[string]string{"zone": "\x1b\x00"},
					}}},
				}}},
			},
			wantZone:      "europe-north2-a",
			wantErrorLine: "europe-north2-a has no capacity for this machine type right now (ZONE_RESOURCE_POOL_EXHAUSTED)",
		},
		{
			name: "zone falls back to a bare operation zone name",
			op: &computepb.Operation{
				Zone:  ptr("europe-north2-a"),
				Error: &computepb.Error{Errors: []*computepb.Errors{{Code: ptr(codePoolExhausted)}}},
			},
			wantZone:      "europe-north2-a",
			wantErrorLine: "europe-north2-a has no capacity for this machine type right now (ZONE_RESOURCE_POOL_EXHAUSTED)",
		},
		{
			name:           "alternative zones tolerate spaces and empty entries",
			op:             capacityOp(codePoolExhausted, "europe-north2-a", "e2-custom-4-8192", "europe-north2-b, europe-north2-c,"),
			wantZone:       "europe-north2-a",
			wantMachine:    "e2-custom-4-8192",
			wantZones:      []string{"europe-north2-b", "europe-north2-c"},
			wantErrorLine:  "europe-north2-a has no capacity for e2-custom-4-8192 right now (ZONE_RESOURCE_POOL_EXHAUSTED)",
			wantMessagePfx: "A e2-custom-4-8192 VM instance",
		},
		{
			name: "a companion error does not hide the capacity one",
			op: &computepb.Operation{
				Zone: ptr("https://www.googleapis.com/compute/v1/projects/p/zones/europe-north2-a"),
				Error: &computepb.Error{Errors: []*computepb.Errors{
					{Code: ptr("QUOTA_EXCEEDED")},
					{Code: ptr(codePoolExhausted)},
				}},
			},
			wantZone:      "europe-north2-a",
			wantErrorLine: "europe-north2-a has no capacity for this machine type right now (ZONE_RESOURCE_POOL_EXHAUSTED)",
		},
		{
			name: "a later detail does not blank what an earlier one supplied",
			op: &computepb.Operation{Error: &computepb.Error{Errors: []*computepb.Errors{{
				Code: ptr(codePoolExhausted),
				ErrorDetails: []*computepb.ErrorDetails{
					{ErrorInfo: &computepb.ErrorInfo{Metadatas: map[string]string{
						"zone": "europe-north2-a", "vmType": "e2-custom-4-8192", "zonesAvailable": "europe-north2-b",
					}}},
					{ErrorInfo: &computepb.ErrorInfo{Metadatas: map[string]string{}}},
				},
			}}}},
			wantZone:      "europe-north2-a",
			wantMachine:   "e2-custom-4-8192",
			wantZones:     []string{"europe-north2-b"},
			wantErrorLine: "europe-north2-a has no capacity for e2-custom-4-8192 right now (ZONE_RESOURCE_POOL_EXHAUSTED)",
		},
		{
			name: "another failure is not capacity",
			op: &computepb.Operation{Error: &computepb.Error{Errors: []*computepb.Errors{{
				Code: ptr("QUOTA_EXCEEDED"),
			}}}},
			wantNil: true,
		},
		{
			name:    "successful operation",
			op:      &computepb.Operation{},
			wantNil: true,
		},
		{
			name:    "nil operation",
			op:      nil,
			wantNil: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := capacityErrorFromOp(tc.op, cause)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("capacityErrorFromOp = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("capacityErrorFromOp = nil, want a capacity error")
			}
			if got.Zone != tc.wantZone {
				t.Errorf("Zone = %q, want %q", got.Zone, tc.wantZone)
			}
			if got.MachineType != tc.wantMachine {
				t.Errorf("MachineType = %q, want %q", got.MachineType, tc.wantMachine)
			}
			if !slices.Equal(got.ZonesAvailable, tc.wantZones) {
				t.Errorf("ZonesAvailable = %v, want %v", got.ZonesAvailable, tc.wantZones)
			}
			if got.Error() != tc.wantErrorLine {
				t.Errorf("Error() = %q, want %q", got.Error(), tc.wantErrorLine)
			}
			if strings.Contains(got.Error(), "\n") {
				t.Errorf("Error() must be a single line, got %q", got.Error())
			}
			if tc.wantMessagePfx != "" && !strings.HasPrefix(got.Message, tc.wantMessagePfx) {
				t.Errorf("Message = %q, want prefix %q", got.Message, tc.wantMessagePfx)
			}
		})
	}
}

// A value of exactly the limit is not cut: the bound is "more than", not "at
// least".
func TestCapacityErrorBoundKeepsExactlyTheLimit(t *testing.T) {
	exact := strings.Repeat("y", maxMessageValue)
	op := &computepb.Operation{Error: &computepb.Error{Errors: []*computepb.Errors{{
		Code:         ptr(codePoolExhausted),
		ErrorDetails: []*computepb.ErrorDetails{{LocalizedMessage: &computepb.LocalizedMessage{Message: ptr(exact)}}},
	}}}}
	got := capacityErrorFromOp(op, nil)
	if got == nil {
		t.Fatal("capacityErrorFromOp = nil, want a capacity error")
	}
	if got.Message != exact {
		t.Errorf("Message was altered: %d runes, want the %d it was given unchanged", len([]rune(got.Message)), maxMessageValue)
	}
}

func TestCapacityErrorTruncatesServerMessage(t *testing.T) {
	long := strings.Repeat("x", maxMessageValue+50)
	op := &computepb.Operation{Error: &computepb.Error{Errors: []*computepb.Errors{{
		Code:         ptr(codePoolExhausted),
		ErrorDetails: []*computepb.ErrorDetails{{LocalizedMessage: &computepb.LocalizedMessage{Message: ptr(long)}}},
	}}}}
	got := capacityErrorFromOp(op, nil)
	if got == nil {
		t.Fatal("capacityErrorFromOp = nil, want a capacity error")
	}
	if want := maxMessageValue + 1; len([]rune(got.Message)) != want {
		t.Errorf("Message length = %d runes, want %d (truncated with an ellipsis)", len([]rune(got.Message)), want)
	}
}

// The bound is in runes, not bytes: a byte cut would split a multi-byte rune and
// put invalid UTF-8 on the terminal, which every ASCII case above would miss.
func TestCapacityErrorTruncatesOnRunesNotBytes(t *testing.T) {
	long := strings.Repeat("ä", maxMessageValue+50)
	op := &computepb.Operation{Error: &computepb.Error{Errors: []*computepb.Errors{{
		Code:         ptr(codePoolExhausted),
		ErrorDetails: []*computepb.ErrorDetails{{LocalizedMessage: &computepb.LocalizedMessage{Message: ptr(long)}}},
	}}}}
	got := capacityErrorFromOp(op, nil)
	if got == nil {
		t.Fatal("capacityErrorFromOp = nil, want a capacity error")
	}
	if want := maxMessageValue + 1; len([]rune(got.Message)) != want {
		t.Errorf("Message length = %d runes, want %d: the bound counts runes, not bytes", len([]rune(got.Message)), want)
	}
	if !utf8.ValidString(got.Message) {
		t.Errorf("Message is not valid UTF-8 (%q): the cut split a rune", got.Message)
	}
}

func TestCapacityErrorUnwrapsToSentinelAndCause(t *testing.T) {
	cause := &googleapi.Error{Code: 503, Message: "SERVICE UNAVAILABLE"}
	err := error(capacityErrorFromOp(capacityOp(codePoolExhausted, "europe-north2-a", "e2-custom-4-8192", ""), cause))
	wrapped := fmt.Errorf("waking: %w", err)

	if !errors.Is(wrapped, ErrNoCapacity) {
		t.Error("errors.Is(err, ErrNoCapacity) = false, want true")
	}
	var gerr *googleapi.Error
	if !errors.As(wrapped, &gerr) || gerr.Code != 503 {
		t.Error("errors.As did not reach the underlying googleapi error")
	}
	var capErr *CapacityError
	if !errors.As(wrapped, &capErr) {
		t.Error("errors.As did not reach the capacity error")
	}
}

func TestCapacityErrorWithDefaults(t *testing.T) {
	c := (&CapacityError{Code: codePoolExhausted}).WithDefaults("europe-north2-a", "e2-custom-4-8192")
	if c.Zone != "europe-north2-a" || c.MachineType != "e2-custom-4-8192" {
		t.Fatalf("WithDefaults did not fill the blanks: %+v", c)
	}
	// Config values are sanitized too, so the one-line Error() survives them.
	dirty := (&CapacityError{Code: codePoolExhausted}).WithDefaults("europe-north2-a\x1b[2J", "e2-custom-4-8192\n")
	if dirty.Zone != "europe-north2-a[2J" || dirty.MachineType != "e2-custom-4-8192" {
		t.Errorf("WithDefaults did not sanitize the config values: %+v", dirty)
	}
	if strings.ContainsAny(dirty.Error(), "\n\r\x1b") {
		t.Errorf("Error() = %q, want no control characters", dirty.Error())
	}
	// Values Compute Engine reported win over the configured ones.
	c = (&CapacityError{Zone: "a", MachineType: "b"}).WithDefaults("europe-north2-a", "e2-custom-4-8192")
	if c.Zone != "a" || c.MachineType != "b" {
		t.Fatalf("WithDefaults overwrote reported values: %+v", c)
	}
}

func TestCapacityErrorFromErr(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantCode    string
		wantMessage string
	}{
		{
			name:        "reason on an error item",
			err:         &googleapi.Error{Code: 503, Errors: []googleapi.ErrorItem{{Reason: codePoolExhausted, Message: "no capacity"}}},
			wantCode:    codePoolExhausted,
			wantMessage: "no capacity",
		},
		{
			name:     "code rendered into the message",
			err:      &googleapi.Error{Code: 503, Message: `SERVICE UNAVAILABLE: errors:{code:"ZONE_RESOURCE_POOL_EXHAUSTED"}`},
			wantCode: codePoolExhausted,
		},
		{
			// A wrapped error is still reached through errors.As, and the quoted
			// form tells the two codes apart, so the longer one reports itself.
			name:     "wrapped with-details code",
			err:      fmt.Errorf("resuming instance: %w", &googleapi.Error{Code: 503, Message: `errors:{code:"ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS"}`}),
			wantCode: codePoolExhaustedWithDetails,
		},
		{
			// The structured reason is trusted at any status; only the message
			// scan is gated on a 503.
			name:     "reason on an error item with another status",
			err:      &googleapi.Error{Code: 400, Errors: []googleapi.ErrorItem{{Reason: codePoolExhausted}}},
			wantCode: codePoolExhausted,
		},
		{
			name: "another 503",
			err:  &googleapi.Error{Code: 503, Message: "SERVICE UNAVAILABLE: backend error"},
		},
		{
			// Prose that merely names the code is not a capacity failure: only
			// the proto-spelled field counts, or an unrelated 503 would buy a
			// five-minute retry loop and a capacity remedy.
			name: "code named in prose rather than as a proto field",
			err:  &googleapi.Error{Code: 503, Message: "SERVICE UNAVAILABLE: still recovering from ZONE_RESOURCE_POOL_EXHAUSTED earlier"},
		},
		{
			// A code named in some other failure's text is not a capacity
			// shortage, and must not be retried as one.
			name: "code mentioned by a non-503",
			err:  &googleapi.Error{Code: 400, Message: `errors:{code:"ZONE_RESOURCE_POOL_EXHAUSTED"}`},
		},
		{
			name: "not a googleapi error",
			err:  errors.New("boom"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := capacityErrorFromErr(tc.err)
			if tc.wantCode == "" {
				if got != nil {
					t.Fatalf("capacityErrorFromErr = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("capacityErrorFromErr = nil, want a capacity error")
			}
			if got.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, tc.wantCode)
			}
			if got.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q", got.Message, tc.wantMessage)
			}
			if !errors.Is(got, ErrNoCapacity) {
				t.Error("classified error does not match ErrNoCapacity")
			}
		})
	}
}

// A value built by hand — the shape Error()'s code guard exists for — reads
// without a trailing empty parenthesis, and still matches the sentinel with no
// underlying cause to unwrap.
func TestCapacityErrorWithoutCode(t *testing.T) {
	c := &CapacityError{Zone: "europe-north2-a"}
	got := c.Error()
	if want := "europe-north2-a has no capacity for this machine type right now"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(c, ErrNoCapacity) {
		t.Error("a CapacityError without a cause does not match ErrNoCapacity")
	}
}

// Code is the only field Error() prints without sanitizing, so the guard admits
// only the two capacity codes rather than any non-empty string: a CapacityError
// built by hand must not be able to put arbitrary text on the terminal.
func TestCapacityErrorIgnoresANonCapacityCode(t *testing.T) {
	c := &CapacityError{Zone: "europe-north2-a", Code: "QUOTA_EXCEEDED"}
	if got := c.Error(); strings.Contains(got, "QUOTA_EXCEEDED") {
		t.Errorf("Error() = %q, want the code omitted: only a capacity code may be printed", got)
	}
}

// Every field comes from the server, so none of them may carry a control
// sequence into the terminal or a newline into the one-line Error().
func TestCapacityErrorSanitizesServerText(t *testing.T) {
	op := &computepb.Operation{Error: &computepb.Error{Errors: []*computepb.Errors{{
		Code: ptr(codePoolExhausted),
		ErrorDetails: []*computepb.ErrorDetails{
			{ErrorInfo: &computepb.ErrorInfo{Metadatas: map[string]string{
				"zone":           "europe-north2-a\u202e\x1b[2J\r",
				"vmType":         "e2-custom-4-8192\n",
				"zonesAvailable": "europe-north2-b\x1b[1m,\teurope-north2-c",
			}}},
			{LocalizedMessage: &computepb.LocalizedMessage{Message: ptr("unavailable\r\nworkbox: fake error")}},
		},
	}}}}

	got := capacityErrorFromOp(op, nil)
	if got == nil {
		t.Fatal("capacityErrorFromOp = nil, want a capacity error")
	}
	if got.Zone != "europe-north2-a[2J" {
		t.Errorf("Zone = %q, want the escape stripped", got.Zone)
	}
	if got.MachineType != "e2-custom-4-8192" {
		t.Errorf("MachineType = %q, want the newline stripped", got.MachineType)
	}
	if want := []string{"europe-north2-b[1m", "europe-north2-c"}; !slices.Equal(got.ZonesAvailable, want) {
		t.Errorf("ZonesAvailable = %q, want %q", got.ZonesAvailable, want)
	}
	if got.Message != "unavailable workbox: fake error" {
		t.Errorf("Message = %q, want the line break turned into a space", got.Message)
	}
	for _, field := range []string{got.Zone, got.MachineType, got.Message, got.Error()} {
		if strings.ContainsAny(field, "\n\r\x1b\t\u202e") {
			t.Errorf("%q still carries a control or format character", field)
		}
	}
}

// A long value from the server is bounded wherever it lands, not just in the
// message.
func TestCapacityErrorBoundsEveryField(t *testing.T) {
	long := strings.Repeat("z", maxMessageValue+50)
	zones := strings.TrimSuffix(strings.Repeat(long+",", maxListEntries+5), ",")
	op := capacityOp(codePoolExhausted, long, long, zones)
	got := capacityErrorFromOp(op, nil)
	if got == nil {
		t.Fatal("capacityErrorFromOp = nil, want a capacity error")
	}
	// The one-line fields keep the short bound; only the server's explanation,
	// which the remedy block prints on its own line, may run longer.
	for name, v := range map[string]string{"Zone": got.Zone, "MachineType": got.MachineType} {
		if n := len([]rune(v)); n > maxRenderedValue+1 {
			t.Errorf("%s is %d runes, want at most %d", name, n, maxRenderedValue+1)
		}
	}
	if n := len([]rune(got.Message)); n > maxMessageValue+1 {
		t.Errorf("Message is %d runes, want at most %d", n, maxMessageValue+1)
	}
	// The zone list is joined onto one line, so its length is bounded per entry
	// and in the number of entries — with a trailing ellipsis marking the cut, as
	// bound does for a long value.
	if len(got.ZonesAvailable) != maxListEntries+1 {
		t.Fatalf("ZonesAvailable has %d entries, want %d kept plus an ellipsis", len(got.ZonesAvailable), maxListEntries)
	}
	if last := got.ZonesAvailable[len(got.ZonesAvailable)-1]; last != "…" {
		t.Errorf("last entry = %q, want the ellipsis marking a cut list", last)
	}
	for _, z := range got.ZonesAvailable {
		if n := len([]rune(z)); n > maxRenderedValue+1 {
			t.Errorf("zone entry is %d runes, want at most %d", n, maxRenderedValue+1)
		}
	}
}
