package ipc

import (
	"encoding/json"
	"testing"
)

// An omitted range field must stay distinguishable from 0: the phone reads an
// explicit 0 as "older than the epoch" or "zero messages" and returns nothing.
func TestSMSConvPayloadKeepsOmittedRangeUnset(t *testing.T) {
	var p SMSConvPayload
	if err := json.Unmarshal([]byte(`{"deviceId":"d","threadID":7}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.RangeStartTimestamp != nil || p.NumberToRequest != nil {
		t.Fatalf("omitted fields decoded as set: %+v", p)
	}
	if err := json.Unmarshal([]byte(`{"deviceId":"d","threadID":7,"rangeStartTimestamp":0,"numberToRequest":0}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.RangeStartTimestamp == nil || p.NumberToRequest == nil {
		t.Fatalf("explicit 0 decoded as omitted: %+v", p)
	}
}
