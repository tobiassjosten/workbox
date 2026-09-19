package state

import "context"

// Fake is an in-memory Store for tests.
type Fake struct {
	Doc *Document
	// Err, when non-nil, is returned by Load, Save, and Clear.
	Err error
}

// NewFake returns an empty in-memory store.
func NewFake() *Fake { return &Fake{Doc: &Document{}} }

func (f *Fake) Load(_ context.Context) (*Document, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	if f.Doc == nil {
		return &Document{}, nil
	}
	return deepCopyDoc(f.Doc), nil
}

func (f *Fake) Save(_ context.Context, doc *Document) error {
	if f.Err != nil {
		return f.Err
	}
	f.Doc = deepCopyDoc(doc)
	return nil
}

func (f *Fake) Clear(_ context.Context) error {
	if f.Err != nil {
		return f.Err
	}
	f.Doc = nil
	return nil
}

var _ Store = (*Fake)(nil)

// deepCopyDoc copies a Document and its span pointers so callers and the fake's
// stored state cannot alias each other — matching the serialise/deserialise
// isolation that the real Firestore store provides.
func deepCopyDoc(d *Document) *Document {
	cp := *d
	cp.Wake = copySpan(d.Wake)
	cp.Sleep = copySpan(d.Sleep)
	cp.Hold = copySpan(d.Hold)
	return &cp
}
