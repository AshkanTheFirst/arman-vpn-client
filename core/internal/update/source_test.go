package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func signVersion(r *release, v string) {
	r.sign()
	r.files[SumsFile] = append(r.files[SumsFile], []byte(versionPrefix+v+"\n")...)
	r.files[SigFile] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(r.priv, r.files[SumsFile])))
}

func TestManifestDownloadUsesExactURLsAndSurvivesRestart(t *testing.T) {
	r := newRelease(t, "1.2.3")
	name := "Arman-VPN-Windows-x64.zip"
	r.files[name] = r.files[AssetName(r.version)]
	delete(r.files, AssetName(r.version))
	signVersion(r, r.version)
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		paths = append(paths, req.URL.RequestURI())
		file := map[string]string{"/metadata/checksums": SumsFile, "/metadata/signature": SigFile, "/fetch": name}[req.URL.Path]
		if file == "" {
			http.NotFound(w, req)
			return
		}
		w.Write(r.files[file])
	}))
	defer server.Close()
	source := Source{name, server.URL + "/fetch?arch=x64", server.URL + "/metadata/checksums", server.URL + "/metadata/signature"}
	appDir, dataDir := appFolder(t, "1.0.0", true)
	opts := Options{DataDir: dataDir, AppDir: appDir, Current: "1.0.0", Transports: func() []http.RoundTripper {
		return []http.RoundTripper{server.Client().Transport}
	}}
	m := New(opts)
	if err := m.DownloadFrom(r.version, source); err != nil {
		t.Fatal(err)
	}
	if s := waitState(t, m); s.State != StateReady {
		t.Fatalf("download: %+v", s)
	}
	if strings.Join(paths, "|") != "/metadata/checksums|/metadata/signature|/fetch?arch=x64" {
		t.Fatalf("requested %v", paths)
	}
	restarted := New(opts)
	if s := restarted.Status(); s.State != StateReady || s.Version != r.version {
		t.Fatalf("restart: %+v", s)
	}
	pkg, err := restarted.checkPackage(r.version)
	if err != nil || filepath.Base(pkg) != name {
		t.Fatalf("package: %q %v", pkg, err)
	}
	unpacked := t.TempDir()
	if err := thePackage.unpack(pkg, unpacked); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(unpacked, thePackage.app)) != "app 1.2.3" {
		t.Fatal("wrong package unpacked")
	}
	// The normal install path must reuse this cached fixed-name archive,
	// preserving the installed copy's settings and uninstaller.
	installed(t, appDir, dataDir, "--hidden")
	var processes fakeProcesses
	processes.install(t)
	if err := Apply(dataDir); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(appDir, thePackage.app)) != "app 1.2.3" ||
		read(t, filepath.Join(dataDir, "config.json")) != "the user's settings" ||
		read(t, filepath.Join(appDir, "unins000.exe")) != "uninstaller" {
		t.Fatal("the fixed-name update did not preserve the installation")
	}
	write(t, pkg, "tampered")
	if _, err := restarted.checkPackage(r.version); err == nil {
		t.Fatal("changed package accepted before installation")
	}
}

func TestFixedPackageNeedsASignedMatchingVersion(t *testing.T) {
	for _, mode := range []string{"matching", "older", "missing", "tampered", "legacy alias"} {
		t.Run(mode, func(t *testing.T) {
			r := newRelease(t, "1.2.3")
			name := "Arman-VPN-Windows-x64.zip"
			r.files[name] = r.files[AssetName(r.version)]
			if mode != "legacy alias" {
				delete(r.files, AssetName(r.version))
			}
			switch mode {
			case "older":
				signVersion(r, "1.1.0")
			case "missing", "legacy alias":
				r.sign()
			default:
				signVersion(r, r.version)
			}
			if mode == "tampered" {
				r.files[SumsFile] = append(r.files[SumsFile], []byte("unsigned change")...)
			}
			srv := r.serve(t)
			base := srv.URL + "/releases/download/v" + r.version + "/"
			appDir, dataDir := appFolder(t, "1.0.0", false)
			m := newManager(appDir, dataDir)
			if err := m.DownloadFrom(r.version, Source{name, base + name, base + SumsFile, base + SigFile}); err != nil {
				t.Fatal(err)
			}
			s := waitState(t, m)
			valid := mode == "matching" || mode == "legacy alias"
			if (s.State == StateReady) != valid {
				t.Fatalf("%s: %+v", mode, s)
			}
			if !valid {
				if _, err := os.Stat(m.versionDir(r.version)); !os.IsNotExist(err) {
					t.Fatalf("failed download retained: %v", err)
				}
			}
		})
	}
}

func TestManifestSourceRejectsUnsafeURLsAndPaths(t *testing.T) {
	base := Source{"package.zip", "https://downloads.example/app", "https://cdn.example/sums", "https://cdn.example/sig"}
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://example.com/app", "file:///tmp/app", "https://user:pass@example.com/app", "https://example.com/app#fragment", ""} {
		source := base
		source.URL = raw
		if source.validate() == nil {
			t.Errorf("accepted URL %q", raw)
		}
	}
	for _, name := range []string{"../package.zip", `..\package.zip`, "C:package.zip", "package.exe", "", "package.zip\n"} {
		source := base
		source.File = name
		if source.validate() == nil {
			t.Errorf("accepted file %q", name)
		}
	}
}
