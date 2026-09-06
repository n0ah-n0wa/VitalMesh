package metrics

import (
	"testing"
	"time"
)

func TestNoopAndOrNoop(t *testing.T) {
	var r Recorder = OrNoop(nil)
	if _, ok := r.(Noop); !ok {
		t.Fatalf("OrNoop(nil) = %T, want Noop", r)
	}
	r.HTTPRequest("GET", "GET /x", 200, time.Millisecond)
	r.Operation("op", OutcomeOK, time.Millisecond)

	custom := Noop{}
	if OrNoop(custom) != custom {
		t.Error("OrNoop replaced a non-nil recorder")
	}
}
