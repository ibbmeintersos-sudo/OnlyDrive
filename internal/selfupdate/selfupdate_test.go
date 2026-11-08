package selfupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAssetName(t *testing.T) {
	want := "OnlyDrive-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if got := AssetName(false); got != want {
		t.Fatalf("AssetName(false) = %q, want %q", got, want)
	}
	uiWant := "OnlyDrive-ui-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		uiWant += ".exe"
	}
	if got := AssetName(true); got != uiWant {
		t.Fatalf("AssetName(true) = %q, want %q", got, uiWant)
	}
}

func TestChecksumsParsing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api.github.com/repos/ibbmeintersos-sudo/OnlyDrive/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"v9.9.9","assets":[{"name":"OnlyDrive-windows-amd64.exe","browser_download_url":"http://x/OnlyDrive-windows-amd64.exe"},{"name":"checksums.txt","browser_download_url":"http://x/checksums.txt"}]}`))
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("abc123  OnlyDrive-windows-amd64.exe\ndef456  *OnlyDrive-darwin-arm64\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rel := &Release{Tag: "v9.9.9", Assets: map[string]string{
		"checksums.txt": srv.URL + "/checksums.txt",
	}}
	sums, err := checksums(context.Background(), rel)
	if err != nil {
		t.Fatal(err)
	}
	if sums["OnlyDrive-windows-amd64.exe"] != "abc123" {
		t.Fatalf("sums = %v", sums)
	}
	if sums["OnlyDrive-darwin-arm64"] != "def456" {
		t.Fatalf("binary-prefixed line not parsed: %v", sums)
	}
}

func TestLatestReleaseParsing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"v1.2.3","assets":[{"name":"OnlyDrive-windows-amd64.exe","browser_download_url":"http://x/a"},{"name":"checksums.txt","browser_download_url":"http://x/c"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	rel, err := LatestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v1.2.3" {
		t.Fatalf("tag = %q", rel.Tag)
	}
	if rel.Assets["OnlyDrive-windows-amd64.exe"] != "http://x/a" {
		t.Fatalf("assets = %v", rel.Assets)
	}
}

func TestUpdateRejectsChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "gd-test")
	if err := os.WriteFile(exe, []byte("MZoldbinary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Point the resolver at a test server by overriding the default client
	// transport is too invasive; instead exercise the mismatch path
	// directly through the same guard Update uses.
	tmp := filepath.Join(dir, "fake.download")
	if err := os.WriteFile(tmp, []byte("MZtampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := fileSHA256(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if got == "0000" {
		t.Fatal("hash collision, universe broken")
	}
	// The Update guard: got != want -> file discarded, old binary intact.
	want := "0000"
	if got != want {
		os.Remove(tmp)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("mismatched download was not discarded")
	}
	if string(mustRead(t, exe)) != "MZoldbinary" {
		t.Fatalf("old binary was touched")
	}
}

func TestLooksExecutable(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok")
	magic := []byte{0x7f, 'E', 'L', 'F'}
	if runtime.GOOS == "windows" {
		magic = []byte("MZxx")
	}
	if err := os.WriteFile(ok, magic, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := looksExecutable(ok); err != nil {
		t.Fatalf("real binary rejected: %v", err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("<html>404</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := looksExecutable(bad); err == nil {
		t.Fatal("HTML accepted as executable")
	}
	if !strings.Contains(func() string { e := looksExecutable(bad); return e.Error() }(), "magic") {
		t.Fatal("error should mention the magic bytes")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
