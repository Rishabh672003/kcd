package events

import "testing"

// All() and the constant block drift apart silently otherwise: a new event
// would be missing from a watch subscriber's default filter list, and the only
// symptom would be an event that never arrives.
func TestAllMatchesConstants(t *testing.T) {
	all := All()
	seen := make(map[EventType]bool, len(all))
	for _, typ := range all {
		if seen[typ] {
			t.Errorf("All() lists %q twice", typ)
		}
		seen[typ] = true
	}

	for _, want := range allTypesFromSource(t) {
		if !seen[want] {
			t.Errorf("All() is missing %q, declared in bus.go", want)
		}
	}
	if len(all) != len(allTypesFromSource(t)) {
		t.Errorf("All() has %d types, bus.go declares %d", len(all), len(allTypesFromSource(t)))
	}
}

// The opt-in split is the reason All() exists: SMS must not reach an
// unfiltered subscriber.
func TestAllExceptOptInDropsOnlyOptInTypes(t *testing.T) {
	got := make(map[EventType]bool)
	for _, typ := range AllExceptOptIn() {
		if got[typ] {
			t.Errorf("AllExceptOptIn() lists %q twice", typ)
		}
		got[typ] = true
	}

	for _, typ := range OptInOnly() {
		if got[typ] {
			t.Errorf("AllExceptOptIn() includes opt-in-only %q", typ)
		}
	}
	for _, typ := range All() {
		if !got[typ] && !contains(OptInOnly(), typ) {
			t.Errorf("AllExceptOptIn() dropped %q, which is not opt-in-only", typ)
		}
	}
	if len(got) != len(All())-len(OptInOnly()) {
		t.Errorf("AllExceptOptIn() has %d types, want %d", len(got), len(All())-len(OptInOnly()))
	}
}

func TestOptInOnlyIsOnlySMS(t *testing.T) {
	if len(OptInOnly()) == 0 {
		t.Fatal("OptInOnly() is empty; SMS would be delivered to unfiltered subscribers")
	}
	for _, typ := range OptInOnly() {
		switch typ {
		case TypeSMSIncoming, TypeSMSAttachment:
		default:
			t.Errorf("OptInOnly() contains %q, which is not an SMS event", typ)
		}
	}
}

func contains(list []EventType, want EventType) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
