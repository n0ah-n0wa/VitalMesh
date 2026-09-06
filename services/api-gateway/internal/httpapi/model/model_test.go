package model

import (
	"encoding/json"
	"testing"
)

func TestNewPageEncoding(t *testing.T) {
	cases := map[string]struct {
		page Page[string]
		want string
	}{
		"nil items, last page": {
			NewPage[string](nil, ""),
			`{"items":[],"next_cursor":null,"has_more":false}`,
		},
		"items with more": {
			NewPage([]string{"a", "b"}, "abc"),
			`{"items":["a","b"],"next_cursor":"abc","has_more":true}`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := json.Marshal(tc.page)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestErrorDetailOmitsEmptyDetails(t *testing.T) {
	got, err := json.Marshal(ErrorResponse{Error: ErrorDetail{Code: "X", Message: "m", RequestID: "r"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"error":{"code":"X","message":"m","request_id":"r"}}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
