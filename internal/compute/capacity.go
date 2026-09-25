package compute

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
)

// Compute Engine's error codes for "this zone cannot provide that machine shape
// right now". Resuming a suspended instance needs live capacity for the exact
// shape — suspending reserves none — and so does starting a terminated one, so
// either path can fail with these.
const (
	codePoolExhausted            = "ZONE_RESOURCE_POOL_EXHAUSTED"
	codePoolExhaustedWithDetails = "ZONE_RESOURCE_POOL_EXHAUSTED_WITH_DETAILS"
)

// ErrNoCapacity marks a failure caused by zone capacity rather than by
// configuration, permissions or instance state. It is transient: callers retry
// on it. Compare with errors.Is; the concrete value is a *CapacityError.
var ErrNoCapacity = errors.New("zone has no capacity for this machine type")

// CapacityError is a capacity failure with whatever the operation's error
// details carried. Every field is best effort — Compute Engine does not
// guarantee any of them — so Error() degrades to the plainest form that is
// still true. Values taken from the server are sanitized as they are classified;
// Zone and MachineType may be empty, in which case a caller that knows the
// configured values (the CLI does) fills them in for the report.
type CapacityError struct {
	// Code is empty or one of the two constants above: both classifiers set it
	// only after matching one. Error() does not rely on that.
	Code string

	Zone           string   // zone that ran out
	MachineType    string   // shape that could not be placed
	ZonesAvailable []string // zones Compute Engine says do have capacity
	Message        string   // Google's own message (the localized one when present)
	err            error    // the underlying API error
}

// Error is a single self-contained line: main() prints errors as "workbox: %s".
// The server's own message can be long, and the remedy block prints it on a line
// of its own (bounded by maxMessageValue), so it stays out of the error value.
func (e *CapacityError) Error() string {
	shape := "this machine type"
	if e.MachineType != "" {
		shape = e.MachineType
	}
	where := "the zone"
	if e.Zone != "" {
		where = e.Zone
	}
	msg := fmt.Sprintf("%s has no capacity for %s right now", where, shape)
	// Both classifiers set a code, and only ever one of the two constants, so it
	// is safe to print as-is. A value built by hand (a test, a caller
	// constructing one for its own report) may carry anything, so check rather
	// than trust.
	if isCapacityCode(e.Code) {
		msg += " (" + e.Code + ")"
	}
	return msg
}

// Unwrap exposes both the sentinel and the API error, so errors.Is finds
// ErrNoCapacity while errors.As still reaches *googleapi.Error the way
// IsPermissionDenied and isNotFound expect.
func (e *CapacityError) Unwrap() []error {
	if e.err == nil {
		return []error{ErrNoCapacity}
	}
	return []error{ErrNoCapacity, e.err}
}

// WithDefaults returns a copy with any missing zone or machine type filled from
// the caller's configuration. Compute Engine usually reports both, but the
// error is more useful when it always names them.
// The configured values are sanitized too, so Error() stays a single bounded line
// whatever the config file contains.
func (e *CapacityError) WithDefaults(zone, machineType string) *CapacityError {
	c := *e
	if c.Zone == "" {
		c.Zone = sanitize(zone)
	}
	if c.MachineType == "" {
		c.MachineType = sanitize(machineType)
	}
	return &c
}

// isCapacityCode reports whether code is one of Compute Engine's capacity
// exhaustion codes.
func isCapacityCode(code string) bool {
	return code == codePoolExhausted || code == codePoolExhaustedWithDetails
}

// capacityErrorFromOp classifies a failed operation from its own proto, which is
// where the structured details live: the client renders the operation's error
// into googleapi.Error.Message as a proto dump, so the zone, the machine type and
// the alternative zones exist in structured form only here; the code can also
// arrive as an error item's reason (see capacityErrorFromErr) and otherwise only
// as text inside that message. It returns nil when the operation did not fail on
// capacity. err is kept as the cause.
func capacityErrorFromOp(op *computepb.Operation, err error) *CapacityError {
	if op == nil {
		return nil
	}
	for _, item := range op.GetError().GetErrors() {
		// An operation can report several errors; only one of them need be the
		// capacity one, so keep looking rather than judging by the first.
		if !isCapacityCode(item.GetCode()) {
			continue
		}
		c := &CapacityError{Code: item.GetCode(), err: err}
		// Details are optional and may repeat; each field takes the first value
		// that survives sanitizing, so a later or emptier detail cannot blank
		// what an earlier one supplied.
		for _, d := range item.GetErrorDetails() {
			m := d.GetErrorInfo().GetMetadatas()
			if c.Zone == "" {
				c.Zone = sanitize(m["zone"])
			}
			if c.MachineType == "" {
				c.MachineType = sanitize(m["vmType"])
			}
			if len(c.ZonesAvailable) == 0 {
				c.ZonesAvailable = splitList(m["zonesAvailable"])
			}
			if c.Message == "" {
				c.Message = sanitizeMessage(d.GetLocalizedMessage().GetMessage())
			}
		}
		// Metadata names the zone that ran out; otherwise the operation's own
		// zone is the only authoritative source. (Errors.location is not one:
		// the API documents it as the request field that caused the error, so a
		// populated value there is a field path such as "zone".)
		if c.Zone == "" {
			c.Zone = sanitize(zoneFromURL(op.GetZone()))
		}
		if c.Message == "" {
			c.Message = sanitizeMessage(item.GetMessage())
		}
		return c
	}
	return nil
}

// capacityErrorFromErr classifies an error returned by the API call itself
// rather than by the operation it started. The operation's structured details
// are absent here, so it reads the reason off the error items and otherwise
// looks for a code in the rendered message — a best-effort fallback to the
// proto path, since capacity failures normally arrive through the operation.
func capacityErrorFromErr(err error) *CapacityError {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return nil
	}
	for _, item := range gerr.Errors {
		if isCapacityCode(item.Reason) {
			return &CapacityError{Code: item.Reason, Message: sanitizeMessage(item.Message), err: err}
		}
	}
	// Only a 503 is read out of the rendered text, and only where the code
	// appears as the proto dump spells it, so an unrelated failure that merely
	// mentions a code is not retried for minutes as a capacity shortage.
	if gerr.Code != http.StatusServiceUnavailable {
		return nil
	}
	// Quoting the code keeps the two apart — code:"…EXHAUSTED" is not part of
	// code:"…EXHAUSTED_WITH_DETAILS" — so either order reports the right one.
	for _, code := range []string{codePoolExhaustedWithDetails, codePoolExhausted} {
		if strings.Contains(gerr.Message, `code:"`+code+`"`) {
			return &CapacityError{Code: code, err: err}
		}
	}
	return nil
}

// splitList parses a comma-separated metadata value, tolerating spaces and empty
// entries and keeping at most maxListEntries of them. A list that was cut ends
// with an ellipsis, the same marker bound uses, so the rendered line does not
// pass for the whole set. SplitSeq avoids materializing an oversized value's
// every comma-separated piece.
func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = sanitize(p); p == "" {
			continue
		}
		if len(out) == maxListEntries {
			return append(out, "…")
		}
		out = append(out, p)
	}
	return out
}

// zoneFromURL takes the zone name out of a resource URL such as
// ".../zones/europe-north2-a"; a bare name passes through unchanged.
func zoneFromURL(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}
