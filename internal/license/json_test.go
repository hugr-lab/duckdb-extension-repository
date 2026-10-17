package license

import (
	"encoding/json"
	"testing"
)

func TestParseJSON(t *testing.T) {
	good := map[string]func(value) bool{
		`{}`:                        func(v value) bool { return v.kind == kindObject && len(v.members) == 0 },
		` [ ] `:                     func(v value) bool { return v.kind == kindArray },
		`{"a":-9007199254740991}`:   func(v value) bool { x, _ := v.get("a"); return x.n == -maxSafe },
		`"\u00e9\ud83d\ude00\n\/"`:  func(v value) bool { return v.s == "\u00e9\U0001F600\n/" },
		`{"a":[true,false,null,0]}`: func(v value) bool { x, _ := v.get("a"); return len(x.arr) == 4 },
		`{"a":1,"b":{"a":2}}`:       func(v value) bool { return len(v.members) == 2 },
	}
	for in, check := range good {
		v, err := parseJSON([]byte(in))
		if err != nil || !check(v) {
			t.Errorf("%s: %+v %v", in, v, err)
		}
	}
	for _, in := range []string{
		``, `{`, `{"a"}`, `{"a":1,}`, `[1,]`, `{"a":1}{}`, `01`, `1.0`, `1e3`, `-`, `9007199254740992`,
		`-9007199254740992`, `"\x"`, `"\u12"`, `"\ud800"`, `"\udc00x"`, "\"\xff\"", "\"\xc0\xaf\"",
		"\"a\nb\"", `{"a":1,"a":1}`, `{"a":1,"\u0061":1}`, "\v{}", `tru`, `nul`, `'a'`,
	} {
		if v, err := parseJSON([]byte(in)); err == nil {
			t.Errorf("%q accepted as %+v", in, v)
		}
	}
}

// FuzzParseJSON: whatever the strict parser accepts, encoding/json accepts too.
func FuzzParseJSON(f *testing.F) {
	for _, s := range []string{`{"a":[1,"x",true]}`, `"😀"`, `-0`, `{"a":{"b":null}}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if _, err := parseJSON(b); err == nil && !json.Valid(b) {
			t.Fatalf("accepted %q, which is not JSON", b)
		}
	})
}
