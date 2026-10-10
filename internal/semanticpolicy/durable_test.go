package semanticpolicy

import (
	"errors"
	"reflect"
	"testing"
)

func TestDurableDecodersReuseClassificationRules(t *testing.T) {
	c, err := DecodeIssueClassification([]byte(`{"type":"Bug","labels":["docs","workflow"],"priority":"High","effort":"S"}`))
	if err != nil || c.Type != Bug || !reflect.DeepEqual(c.Labels, []Label{"workflow", "docs"}) {
		t.Fatal(c, err)
	}
	for _, tc := range []struct {
		raw string
		err error
	}{
		{`{"type":"Task","labels":[],"priority":"High","effort":"S","estimate":1}`, ErrUnknown},
		{`{"type":"Task","labels":[],"priority":"High"}`, ErrMissing},
		{`{"type":"Task","type":"Bug","labels":[],"priority":"High","effort":"S"}`, ErrDuplicate},
		{`{"type":"Unknown","labels":[],"priority":"High","effort":"S"}`, ErrIssueType},
	} {
		if _, err := DecodeIssueClassification([]byte(tc.raw)); !errors.Is(err, tc.err) {
			t.Fatal(err)
		}
	}
	p, err := DecodePRClassification([]byte(`{"labels":["docs","workflow"]}`))
	if err != nil || !reflect.DeepEqual(p.Labels, []Label{"workflow", "docs"}) {
		t.Fatal(p, err)
	}
	for _, raw := range []string{`{"labels":null}`, `{"labels":["help wanted"]}`, `{"labels":[],"Target":"#3"}`, `{"labels":[],"labels":[]}`} {
		if _, err := DecodePRClassification([]byte(raw)); err == nil {
			t.Fatal("invalid PR record accepted", raw)
		}
	}
}
