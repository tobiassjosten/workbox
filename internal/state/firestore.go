package state

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/tobiassjosten/workbox/internal/schedule"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// wireSpan is the Firestore representation of a schedule span. The field names
// are the contract with the Workflow reconciler (infra/reconcile.yaml.tftpl).
type wireSpan struct {
	Start time.Time `firestore:"start"`
	End   time.Time `firestore:"end"`
	State string    `firestore:"state"`
}

func toWire(s *schedule.Span) *wireSpan {
	if s == nil {
		return nil
	}
	return &wireSpan{Start: s.Start, End: s.End, State: string(s.State)}
}

// parseDesired converts a wire state string to the typed Desired value.
// It returns ok=false for unknown or empty strings, rejecting bad wire data.
func parseDesired(s string) (schedule.Desired, bool) {
	d := schedule.Desired(s)
	if d != schedule.Awake && d != schedule.Asleep {
		return "", false
	}
	return d, true
}

func fromWire(w *wireSpan) *schedule.Span {
	if w == nil {
		return nil
	}
	d, ok := parseDesired(w.State)
	if !ok {
		return nil
	}
	return &schedule.Span{Start: w.Start, End: w.End, State: d}
}

type wireDoc struct {
	Wake      *wireSpan `firestore:"wake_override"`
	Sleep     *wireSpan `firestore:"sleep_override"`
	Hold      *wireSpan `firestore:"hold"`
	UpdatedBy string    `firestore:"updated_by"`
	UpdatedAt time.Time `firestore:"updated_at"`
}

// Firestore is a Store backed by a single Firestore document.
type Firestore struct {
	client     *firestore.Client
	collection string
	document   string
	updater    string
	Now        func() time.Time // nil means time.Now
}

func (f *Firestore) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// FirestoreConfig identifies the target document.
type FirestoreConfig struct {
	Project    string
	Database   string
	Collection string
	Document   string
	// Updater is recorded in updated_by (e.g. the local username).
	Updater string
}

// NewFirestore builds a Firestore-backed store using ADC.
func NewFirestore(ctx context.Context, cfg FirestoreConfig, opts ...option.ClientOption) (*Firestore, error) {
	db := cfg.Database
	if db == "" {
		db = "(default)"
	}
	client, err := firestore.NewClientWithDatabase(ctx, cfg.Project, db, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating firestore client: %w", err)
	}
	return &Firestore{
		client:     client,
		collection: cfg.Collection,
		document:   cfg.Document,
		updater:    cfg.Updater,
	}, nil
}

// Close releases the underlying client.
func (f *Firestore) Close() error { return f.client.Close() }

func (f *Firestore) ref() *firestore.DocumentRef {
	return f.client.Collection(f.collection).Doc(f.document)
}

// Load returns the current document, or an empty document if none exists.
func (f *Firestore) Load(ctx context.Context) (*Document, error) {
	snap, err := f.ref().Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return &Document{}, nil
		}
		return nil, fmt.Errorf("reading state: %w", err)
	}
	var wd wireDoc
	if err := snap.DataTo(&wd); err != nil {
		return nil, fmt.Errorf("decoding state: %w", err)
	}
	return &Document{
		Wake:      fromWire(wd.Wake),
		Sleep:     fromWire(wd.Sleep),
		Hold:      fromWire(wd.Hold),
		UpdatedBy: wd.UpdatedBy,
		UpdatedAt: wd.UpdatedAt,
	}, nil
}

// Save writes the document, replacing any existing one.
func (f *Firestore) Save(ctx context.Context, doc *Document) error {
	wd := wireDoc{
		Wake:      toWire(doc.Wake),
		Sleep:     toWire(doc.Sleep),
		Hold:      toWire(doc.Hold),
		UpdatedBy: f.updater,
		UpdatedAt: f.now().UTC(),
	}
	if _, err := f.ref().Set(ctx, wd); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}
	return nil
}

// Clear removes all overrides and holds by deleting the state document so the
// reconciler's garbage collector does not leave a stale empty document.
func (f *Firestore) Clear(ctx context.Context) error {
	_, err := f.ref().Delete(ctx)
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("clearing state: %w", err)
	}
	return nil
}

var _ Store = (*Firestore)(nil)
