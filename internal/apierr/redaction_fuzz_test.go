package apierr_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/apierr"
)

// A typed error has a public message and a private cause. Exercise Write,
// including its traversal of wrapped errors, not Error() (which is diagnostic).
func FuzzErrorCauseNotRendered(f *testing.F) {
	const marker = "TESTONLY_PRIVATE_CAUSE_"
	const public = "request could not be completed"
	f.Add("Bearer opaque", 401, false)
	f.Add("\xff\x00\"&", 500, true)
	// Every status TypeForStatus maps, so that a cause rendered for just one
	// error type (or one status in a shared case) fails on the seed corpus
	// that CI runs, not only under random search. 408/504 share a type, as do
	// 503/529; 500, 502 and 599 fall through to the api_error default.
	for i, status := range []int{400, 401, 402, 403, 404, 408, 409, 413, 429, 500, 502, 503, 504, 529, 599} {
		f.Add("Bearer opaque", status, false)
		f.Add("cause: \xff\"&", status, true)
		f.Add("", status, i%2 == 0)
	}
	f.Fuzz(func(t *testing.T, detail string, status int, wrapped bool) {
		if len(detail) > 4096 {
			t.Skip()
		}
		typ := apierr.TypeForStatus(status)
		var err error = apierr.Wrap(errors.New(marker+detail), typ, public)
		if wrapped {
			err = fmt.Errorf("internal context %s: %w", marker+detail, err)
		}
		w := httptest.NewRecorder()
		if err := apierr.Write(w, err); err != nil {
			t.Fatal(err)
		}
		var env struct {
			Type  string
			Error struct{ Type, Message string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if w.Code != apierr.StatusFor(typ) || env.Type != "error" || env.Error.Type != string(typ) || env.Error.Message != public {
			t.Fatal("response did not render the public typed error")
		}
		if strings.Contains(w.Body.String(), marker) {
			t.Fatal("response disclosed the private error cause or wrapping context")
		}
	})
}
