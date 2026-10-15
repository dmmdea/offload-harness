package research

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ytdlp_sha256: when the operator sets it, the file at ytdlp_path is hashed before every exec and a mismatch refuses
// with ErrKindYtDlpPath, running nothing. Unset, nothing is hashed.

func fileHashHex(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// countHashing makes the adapter's file hashing observable.
func countHashing(t *testing.T) *int {
	t.Helper()
	real := hashFile
	n := 0
	hashFile = func(path string) (string, error) { n++; return real(path) }
	t.Cleanup(func() { hashFile = real })
	return &n
}

func TestYtDlpSHA256MatchRunsTheBinary(t *testing.T) {
	pinYouTube(t)
	want := fileHashHex(t, fakeBinary(t))
	for name, value := range map[string]string{
		"lower case":           want,
		"upper case":           strings.ToUpper(want),
		"surrounding spaces":   "  " + want + "\t",
		"upper and with space": " " + strings.ToUpper(want) + " ",
	} {
		t.Run(name, func(t *testing.T) {
			opt, root := ytOpts(t)
			opt.YtDlpSHA256 = value
			got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt)
			if got.Err != "" || got.Text == "" {
				t.Fatalf("a matching hash must not stop the run: %+v", got)
			}
			if n := len(readCalls(t, root)); n != 1 {
				t.Errorf("binary invoked %d times, want 1", n)
			}
		})
	}
}

func TestYtDlpSHA256MismatchRefusesWithoutRunningAnything(t *testing.T) {
	pinYouTube(t)
	right := fileHashHex(t, fakeBinary(t))
	wrong := strings.Repeat("0", 64)
	if wrong == right {
		t.Fatal("test premise")
	}
	opt, root := ytOpts(t)
	opt.YtDlpSHA256 = wrong
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt)
	if got.ErrKind != ErrKindYtDlpPath || got.Text != "" {
		t.Fatalf("got %+v, want a %s refusal and no text", got, ErrKindYtDlpPath)
	}
	if !strings.Contains(got.Err, "ytdlp_sha256") || !strings.Contains(got.Err, right) {
		t.Errorf("the refusal should name the setting and the hash the file really has (so an operator can pin it): %q", got.Err)
	}
	if n := len(readCalls(t, root)); n != 0 {
		t.Errorf("binary invoked %d times after a hash mismatch", n)
	}
	if left := leftovers(t, root); len(left) != 0 {
		t.Errorf("a refused run left %v behind", left)
	}
}

func TestYtDlpSHA256MalformedValueRefusesToo(t *testing.T) {
	pinYouTube(t)
	right := fileHashHex(t, fakeBinary(t))
	hashed := countHashing(t)
	for _, bad := range []string{"deadbeef", right[:63], right + "0", strings.Repeat("g", 64), "sha256:" + right} {
		opt, root := ytOpts(t)
		opt.YtDlpSHA256 = bad
		got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt)
		if got.ErrKind != ErrKindYtDlpPath || got.Text != "" || !strings.Contains(got.Err, "ytdlp_sha256") || !strings.Contains(got.Err, "64 hexadecimal digits") {
			t.Errorf("value %q: got %+v, want a %s refusal that says the value is not a sha256 (64 hexadecimal digits)", bad, got, ErrKindYtDlpPath)
		}
		if n := len(readCalls(t, root)); n != 0 {
			t.Errorf("value %q: binary invoked %d times", bad, n)
		}
	}
	if *hashed != 0 {
		t.Errorf("the file was hashed %d times for values that cannot be a hash", *hashed)
	}
}

func TestYtDlpIsHashedBeforeEveryExecAndOnlyWhenPinned(t *testing.T) {
	pinYouTube(t)
	hashed := countHashing(t)

	// unset: nothing is hashed
	opt, _ := ytOpts(t)
	if got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt); got.Err != "" {
		t.Fatalf("fetch failed: %s", got.Err)
	}
	if *hashed != 0 {
		t.Errorf("hashed %d times with ytdlp_sha256 unset, want 0", *hashed)
	}

	// a binary replaced between two calls is caught by the second one
	exe, err := os.ReadFile(fakeBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "yt-dlp"+filepath.Ext(fakeBinary(t)))
	if err := os.WriteFile(bin, exe, 0o755); err != nil {
		t.Fatal(err)
	}
	opt, root := ytOpts(t)
	opt.YtDlpPath = bin
	opt.YtDlpSHA256 = fileHashHex(t, bin)
	if got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt); got.Err != "" {
		t.Fatalf("first call: %s", got.Err)
	}
	if *hashed != 1 {
		t.Errorf("hashed %d times for one call, want 1", *hashed)
	}
	// The file is replaced, not written to: Linux refuses to open for writing a binary that was running a moment ago
	// ("text file busy"), and the kernel lets go of it a little after the process is reaped.
	replacement := bin + ".new"
	if err := os.WriteFile(replacement, append(exe[:len(exe):len(exe)], "tampered"...), 0o755); err != nil {
		t.Fatal(err)
	}
	var rerr error
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if rerr = os.Rename(replacement, bin); rerr == nil {
			break
		}
	}
	if rerr != nil {
		t.Fatalf("replacing the binary: %v", rerr)
	}
	got := Fetch(context.Background(), "https://www.youtube.com/watch?v="+idPlain, opt)
	if got.ErrKind != ErrKindYtDlpPath || got.Text != "" {
		t.Fatalf("second call after the file changed: %+v, want a %s refusal", got, ErrKindYtDlpPath)
	}
	if *hashed != 2 {
		t.Errorf("hashed %d times for two calls, want 2 (before every exec)", *hashed)
	}
	if n := len(readCalls(t, root)); n != 1 {
		t.Errorf("binary invoked %d times, want only the first call's", n)
	}
}
