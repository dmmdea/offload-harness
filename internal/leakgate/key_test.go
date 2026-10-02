package leakgate

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// keyEnv drives ResolveKey from a table: no test in this package reads or sets
// a real environment variable.
type keyEnv struct {
	env   map[string]string
	files map[string]string
	errs  map[string]error
	reads []string
}

func (k *keyEnv) getenv(name string) string { return k.env[name] }

func (k *keyEnv) readFile(path string) ([]byte, error) {
	k.reads = append(k.reads, path)
	if err, ok := k.errs[path]; ok {
		return nil, err
	}
	if s, ok := k.files[path]; ok {
		return []byte(s), nil
	}
	return nil, fs.ErrNotExist
}

const defaultKeyPath = "cfg/offload-harness/leak-gate.key"

func TestLeakGateKeyResolution(t *testing.T) {
	good := hexKey("ab")
	other := hexKey("cd")
	prev := hexKey("ef")
	rows := []struct {
		name       string
		env        map[string]string
		files      map[string]string
		errs       map[string]error
		defaultSet bool
		wantKey    string
		wantPrev   string
		wantErr    error
		noRead     bool // the file reader must not be consulted at all
	}{
		{name: "nothing set", defaultSet: true},
		{name: "KEY valid", env: map[string]string{EnvKey: good}, defaultSet: true, wantKey: good, noRead: true},
		{name: "KEY valid with surrounding whitespace and a CRLF", env: map[string]string{EnvKey: "  " + good + "\r\n"}, wantKey: good},
		{name: "KEY empty (an unset Actions secret) is unset, not malformed", env: map[string]string{EnvKey: ""}, defaultSet: true},
		{name: "KEY whitespace-only is unset", env: map[string]string{EnvKey: " \t\r\n "}, defaultSet: true},
		{name: "KEY too short is malformed", env: map[string]string{EnvKey: good[:63]}, wantErr: ErrMalformedKey},
		{name: "KEY too long is malformed", env: map[string]string{EnvKey: good + "a"}, wantErr: ErrMalformedKey},
		{name: "KEY non-hex is malformed", env: map[string]string{EnvKey: strings.Repeat("zz", 32)}, wantErr: ErrMalformedKey},
		{name: "KEY wins over KEY_FILE and the file is never read", env: map[string]string{EnvKey: good, EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": other}, wantKey: good, noRead: true},
		{name: "KEY empty falls through to KEY_FILE", env: map[string]string{EnvKey: "", EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": other}, wantKey: other},
		{name: "KEY_FILE CRLF-terminated", env: map[string]string{EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": other + "\r\n"}, wantKey: other},
		{name: "KEY_FILE set but absent is fatal", env: map[string]string{EnvKeyFile: "k.txt"}, wantErr: ErrKeyFile},
		{name: "KEY_FILE set but unreadable is fatal", env: map[string]string{EnvKeyFile: "k.txt"}, errs: map[string]error{"k.txt": errors.New("denied")}, wantErr: ErrKeyFile},
		{name: "KEY_FILE set but malformed is fatal", env: map[string]string{EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": "not a key"}, wantErr: ErrKeyFile},
		{name: "KEY_FILE empty file is fatal (the operator asked for that file)", env: map[string]string{EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": ""}, wantErr: ErrKeyFile},
		{name: "KEY_FILE empty value is not consulted", env: map[string]string{EnvKeyFile: ""}, noRead: true},
		{name: "KEY_FILE whitespace-only value is not consulted", env: map[string]string{EnvKeyFile: "  "}, noRead: true},
		{name: "default file absent is unset", defaultSet: true},
		{name: "default file present", files: map[string]string{defaultKeyPath: good}, defaultSet: true, wantKey: good},
		{name: "default file present, CRLF and a trailing blank line", files: map[string]string{defaultKeyPath: good + "\r\n\r\n"}, defaultSet: true, wantKey: good},
		{name: "default file present but malformed is fatal", files: map[string]string{defaultKeyPath: "short"}, defaultSet: true, wantErr: ErrKeyFile},
		{name: "default file unreadable is fatal", errs: map[string]error{defaultKeyPath: errors.New("denied")}, defaultSet: true, wantErr: ErrKeyFile},
		{name: "KEY_FILE wins over the default file", env: map[string]string{EnvKeyFile: "k.txt"}, files: map[string]string{"k.txt": other, defaultKeyPath: good}, defaultSet: true, wantKey: other},
		{name: "no default path is not consulted", files: map[string]string{defaultKeyPath: good}},
		{name: "6a KEY valid and KEY_PREV empty", env: map[string]string{EnvKey: good, EnvKeyPrev: ""}, wantKey: good},
		{name: "6a KEY valid and KEY_PREV whitespace-only", env: map[string]string{EnvKey: good, EnvKeyPrev: "  \n"}, wantKey: good},
		{name: "KEY and KEY_PREV valid", env: map[string]string{EnvKey: good, EnvKeyPrev: prev}, wantKey: good, wantPrev: prev},
		{name: "6b KEY_PREV malformed is fatal", env: map[string]string{EnvKey: good, EnvKeyPrev: "bad"}, wantErr: ErrMalformedPrevKey},
		{name: "6b KEY_PREV too long is fatal", env: map[string]string{EnvKey: good, EnvKeyPrev: prev + "0"}, wantErr: ErrMalformedPrevKey},
		{name: "6c KEY_PREV never counts as the key", env: map[string]string{EnvKeyPrev: prev}, defaultSet: true},
		{name: "6c KEY_PREV garbage without a key is ignored", env: map[string]string{EnvKeyPrev: "garbage"}, defaultSet: true},
		{name: "a key from the file with a previous key", env: map[string]string{EnvKeyFile: "k.txt", EnvKeyPrev: prev}, files: map[string]string{"k.txt": other}, wantKey: other, wantPrev: prev},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			ke := &keyEnv{env: r.env, files: r.files, errs: r.errs}
			def := ""
			if r.defaultSet {
				def = defaultKeyPath
			} else if _, ok := r.files[defaultKeyPath]; ok && r.name == "no default path is not consulted" {
				def = ""
			}
			k, err := ResolveKey(ke.getenv, ke.readFile, def)
			if r.wantErr != nil {
				if !errors.Is(err, r.wantErr) {
					t.Fatalf("err = %v, want %v", err, r.wantErr)
				}
				assertNoKeyMaterial(t, err, good, other, prev)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := hexOf(k.Key); got != r.wantKey {
				t.Errorf("key = %q, want %q", got, r.wantKey)
			}
			if got := hexOf(k.Prev); got != r.wantPrev {
				t.Errorf("prev = %q, want %q", got, r.wantPrev)
			}
			if r.noRead && len(ke.reads) != 0 {
				t.Errorf("the file reader was consulted: %v", ke.reads)
			}
		})
	}
}

func hexOf(b []byte) string {
	if b == nil {
		return ""
	}
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

// assertNoKeyMaterial: an error never carries key text, a fragment of it, or a
// length (5.9: "never print the key, its length, the expected MAC").
func assertNoKeyMaterial(t *testing.T, err error, keys ...string) {
	t.Helper()
	msg := err.Error()
	for _, k := range keys {
		if strings.Contains(msg, k) || strings.Contains(msg, k[:16]) {
			t.Errorf("error carries key text: %q", msg)
		}
	}
	for _, bad := range []string{"64", "63", "65", "length", "expected", "bytes"} {
		if strings.Contains(strings.ToLower(msg), bad) {
			t.Errorf("error mentions %q (a hint about the key): %q", bad, msg)
		}
	}
}

func TestMalformedKeyErrorsNameNothingAboutTheKey(t *testing.T) {
	secret := hexKey("ab")
	for _, v := range []string{secret[:40], secret + "00", "xyz" + secret[3:]} {
		ke := &keyEnv{env: map[string]string{EnvKey: v}}
		_, err := ResolveKey(ke.getenv, ke.readFile, "")
		if !errors.Is(err, ErrMalformedKey) {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), v) {
			t.Errorf("error repeats the malformed value: %q", err)
		}
	}
	if ErrMalformedKey.Error() != "malformed gate key" || ErrMalformedPrevKey.Error() != "malformed previous gate key" {
		t.Errorf("the two messages the design pins changed: %q / %q", ErrMalformedKey, ErrMalformedPrevKey)
	}
}

func TestParseRequired(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"1", true, false},
		{"true", false, true},
		{"0", false, true},
		{"yes", false, true},
		{" 1", false, true},
		{"1 ", false, true},
		{"11", false, true},
		{" ", false, true},
	} {
		got, err := ParseRequired(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("ParseRequired(%q) = %v, %v; want %v, err=%v", c.in, got, err, c.want, c.wantErr)
		}
		if err != nil && !errors.Is(err, ErrBadRequired) {
			t.Errorf("ParseRequired(%q) error %v is not ErrBadRequired", c.in, err)
		}
	}
	if !strings.Contains(ErrBadRequired.Error(), "unrecognized OFFLOAD_LEAK_GATE_REQUIRED value") {
		t.Errorf("message = %q", ErrBadRequired)
	}
}

// TestDecideRows covers rows 1, 4 and 6 of the behaviour matrix: with a key the
// gate enforces; without one it is fatal where secrets exist and a visible skip
// elsewhere.
func TestDecideRows(t *testing.T) {
	withKey := Keys{Key: keyBytes("ab")}
	d, err := Decide(withKey, false)
	if err != nil || !d.Enforce || d.SkipReason != "" {
		t.Errorf("key, not required: %+v, %v", d, err)
	}
	d, err = Decide(withKey, true)
	if err != nil || !d.Enforce {
		t.Errorf("key, required: %+v, %v", d, err)
	}
	if _, err = Decide(Keys{}, true); !errors.Is(err, ErrKeyMissing) {
		t.Errorf("no key, required: err = %v, want ErrKeyMissing", err)
	}
	d, err = Decide(Keys{}, false)
	if err != nil || d.Enforce || d.SkipReason == "" {
		t.Errorf("no key, not required: %+v, %v (want a skip with a reason)", d, err)
	}
	// A previous key alone never counts as the key (row 6c).
	if _, err = Decide(Keys{Prev: keyBytes("ef")}, true); !errors.Is(err, ErrKeyMissing) {
		t.Errorf("previous key alone: err = %v", err)
	}
}

func TestDecodeKey(t *testing.T) {
	if _, err := DecodeKey(hexKey("ab")); err != nil {
		t.Errorf("valid key: %v", err)
	}
	if _, err := DecodeKey(strings.ToUpper(hexKey("ab"))); err != nil {
		t.Errorf("upper-case key: %v", err)
	}
	for _, bad := range []string{"", "ab", hexKey("ab") + "0", strings.Repeat("g", 64), hexKey("ab")[:63] + "\n"} {
		if _, err := DecodeKey(bad); err == nil {
			t.Errorf("DecodeKey(%q) accepted a malformed key", bad)
		}
	}
}
